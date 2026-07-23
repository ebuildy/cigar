# Interactive report commands — design

**Date:** 2026-07-22
**Status:** Approved (design), pending implementation plan

## Summary

Let users interact with the bot's MR resource report by **replying to the report
comment** with commands. The first two commands:

- `help` — reply listing all available commands.
- `details [job|pod] <name>` — reply with three charts (CPU, memory, network) of
  the target's resource usage over its run window, rendered as **SVG** images
  embedded in the reply.

The feature is **off by default** and gated by `COMMANDS_ENABLED`. Further
commands will be added later, so the command layer is built to extend.

Command grammar (case-insensitive, applied to the first non-empty line, trimmed):

- `^help$`
- `^details\s+(?:(job|pod)\s+)?(\S+)$` — when the `job`/`pod` type word is
  omitted, auto-detect: a name matching `^runner-` is a **pod**, otherwise a
  **job**. (Runner build pods follow `runner-<token>-project-<id>-concurrent-<n>`.)

## Security model (non-negotiable)

The bot has broad Prometheus access. A reply command must never let a user read
metrics for a pod/job they cannot already see, and must not be defeatable by
editing comments. Two edit-based attacks are in scope and both are closed:

- **Marker spoofing** — pasting the `<!-- ci-resources-bot -->` marker into one's
  own comment (or editing another comment to add it) to masquerade as the report.
- **Allowlist tampering** — editing the bot's report note to widen what pods are
  "allowed".

Defenses, in order of evaluation (any failure → drop silently, no reply):

1. **Author gate (unspoofable).** The command note's *discussion root* must be
   authored by the bot's own GitLab user id (resolved once at startup via
   `GET /user`). A note's author cannot be changed by editing, so a spoofed
   marker on someone else's comment fails here. The `<!-- ci-resources-bot -->`
   marker text is a lookup hint, never the security boundary.
2. **Loop guard (marker-based, identity-free).** The bot skips any note whose
   body carries the `<!-- ci-resources-bot` marker — every note the bot writes
   (report *and* command replies) is tagged with it. This is deliberately **not**
   an `author == bot` check: the GitLab token may belong to a real user who also
   posts commands, so an identity check would wrongly drop that user's replies
   (and silently — the original bug). A human pasting the marker into their own
   comment only self-suppresses; it grants nothing.
3. **Authoritative context.** Project id and MR IID come from the **webhook
   payload**, never from note content. Every GitLab/Prometheus query is scoped to
   that project — a target can never reach another project's pipelines or pods.
4. **HMAC-signed marker (tamper detection).** The report note body carries a
   signed marker:

   ```
   <!-- ci-resources-bot p=<pipelineID> m=<mrIID> sig=<hex(HMAC-SHA256(key, "p=<pipelineID>;m=<mrIID>"))> -->
   ```

   On a command, recompute the HMAC over the parsed `p`/`m` with
   `COMMANDS_SIGNING_KEY` and constant-time compare. Missing/invalid signature →
   drop. This pins the exact pipeline the report is about and rejects any edit
   that alters `p`/`m`. GitLab's notes API cannot tell us "a human edited this"
   (the bot itself edits the note on every pipeline run via the idempotent
   upsert, so `updated_at` is not a usable signal) — the HMAC is what makes the
   "not edited" guarantee reliable.
5. **Live allowlist (edit-proof).** The set of permitted targets is derived from
   **authoritative GitLab data**, not the note body: list the signed pipeline's
   jobs (`GET /projects/<payloadProject>/pipelines/<p>/jobs`) and correlate each
   to its pod via the existing `correlate.Resolver`.
   - `details job X` → `X` must be a job name in that list.
   - `details pod X` → `X` must be a pod the bot **itself** resolved from one of
     those jobs. An attacker-supplied pod name is never passed straight into a
     Prometheus query.

   A target outside the allowlist → a short "not part of this report" reply.

Because authorization derives from the payload's project + the signed pipeline +
live correlation — and never from the note's free text — editing the report note
cannot grant access to anything.

## Architecture

Integration approach: **shared queue, tagged event, new command package.** The
existing `webhook → queue → worker → reporter` pipeline is generalized minimally
rather than duplicated.

```txt
webhook: accept "Pipeline Hook" + (when enabled) "Note Hook"
   |
   v
queue chan Event { Pipeline *PipelineEvent | Note *NoteEvent }
   |
   v
worker: switch on event kind
   |-- Pipeline -> reporter.ProcessPipeline   (unchanged)
   `-- Note     -> command.Handle

new: internal/command   parse + authorize + execute
new: internal/chart     time series -> sanitizer-safe SVG
metrics: + range queries (PodSeries), + PodActiveSpan
gitlab:  + CurrentUser, MergeRequestDiscussion, PipelineJobsByName,
           UploadFile, CreateDiscussionReply
report:  marker split into lookup prefix + signed marker
config:  + COMMANDS_ENABLED, + COMMANDS_SIGNING_KEY
```

### Component boundaries

- **`internal/webhook`** — accepts `Note Hook` only when `COMMANDS_ENABLED`. Does
  a **cheap, pure-CPU syntactic pre-filter** (no I/O, honoring "the handler only
  validates/filters/enqueues"): parse `object_attributes.note`; enqueue only when
  it matches a known command grammar **and** `object_attributes.noteable_type ==
  "MergeRequest"`. Non-matching or non-MR notes → `200` ignore, never enqueued.
  All semantic authorization (author gate, HMAC, allowlist) happens later in the
  worker because it needs GitLab API calls. The handler still must not talk to
  Prometheus or GitLab.
- **`internal/command`** — parse, authorize (security model above), execute.
  Depends on `gitlab.Client`, `metrics.Source`, `correlate.Resolver`,
  `chart` renderer, and the HMAC key. Returns after posting (at most) one reply.
- **`internal/chart`** — pure `text/template` line-chart renderer: a labeled time
  series → a self-contained, sanitizer-safe SVG (no `<script>`, no external refs,
  inline presentation attributes only) so GitLab renders the upload.
- **`internal/metrics`** — new range/aggregation queries (below); unchanged for
  the pipeline report path.
- **`internal/gitlab`** — new client methods (below).
- **`internal/report`** — marker split (below).

## Event routing & queue

- `webhook.Event` is a tagged union carrying either `*PipelineEvent` or
  `*NoteEvent`. `Enqueuer.Enqueue(Event) bool`; the bounded in-memory queue in
  `serve.go` becomes `chan webhook.Event`. `Enqueue` still never blocks.
- `NoteEvent` carries the minimal fields the worker needs from the `Note Hook`
  payload: project id, MR IID, discussion id, note id, note body, author id/kind.
- Worker `switch`es on the kind: pipeline → `reporter.ProcessPipeline`
  (unchanged), note → `command.Handle`.
- **Dedup:** retried `Note Hook` deliveries are deduped by note id (in-memory
  seen-set, mirroring the existing pipeline dedupe by `pipeline.id` + `status`),
  so a redelivery never double-replies.

## `internal/command`

```go
// Handle authorizes and executes a single command note. It performs at most one
// reply and swallows unauthorized notes silently.
func (h *Handler) Handle(ctx context.Context, ev webhook.NoteEvent) error
```

Flow:

1. Parse the note body → `nil` (ignore) or a `Command` (`help`, or
   `details{Type, Name}`).
2. Loop guard: `ev.AuthorID == h.botUserID` → drop.
3. Fetch the note's **discussion**; take its root note. Author gate: root author
   == `botUserID`, else drop. Parse+verify the root note's **signed marker**
   (HMAC over `p`,`m` with the signing key); on mismatch/missing → drop. Confirm
   `m` matches `ev.MRIID`.
4. Execute:
   - **help** → post a reply listing commands (static template).
   - **details** → resolve target against the **live allowlist** for pipeline `p`
     in the payload's project:
     - *job*: find the job by name in the pipeline; window =
       `started_at..finished_at`; correlate to pod via `Resolver`.
     - *pod*: confirm the pod is one correlated from a job of pipeline `p`;
       window = `metrics.PodActiveSpan(pod)`.
     - Query `metrics.PodSeries` for CPU, memory, network over the window →
       render 3 SVGs → `UploadFile` each → one `CreateDiscussionReply` embedding
       all three (with a heading naming the target and window).
5. Errors surfaced to the user as short replies (target not found / not part of
   this report / no metrics for the window). Non-command replies in the thread →
   silent ignore (likely human conversation).

`details` replies are **not** idempotent — each command yields one fresh reply
(only the main report note stays idempotent). Repeated commands are user-driven
and low volume.

## `internal/metrics` additions

```go
// PodSeries returns aligned time series for a pod over [start,end] using
// Prometheus query_range. Step is derived from the window (>= scrape interval).
// Absent series -> empty series (never fabricated as zero).
func (s Source) PodSeries(ctx, pod string, start, end time.Time) (PodSeries, error)

// PodActiveSpan returns the min/max sample timestamps where the pod's cadvisor
// series exist, for the `details pod` window. Absent -> ok=false.
func (s Source) PodActiveSpan(ctx, pod string) (start, end time.Time, ok bool, err error)
```

`PodSeries` carries three labeled series:

- **CPU** — `rate(container_cpu_usage_seconds_total{pod,container!="",container!="POD"}[step])`, summed.
- **Memory** — `sum(container_memory_working_set_bytes{pod,container!="",container!="POD"})`.
- **Network** — `rate(container_network_receive_bytes_total{pod}[step])` and the
  transmit equivalent (two lines, pod-level, no `container` label).

Same conventions as the existing report queries: exclude the pause/`POD`
container, absent series ≠ zero, windows padded by one scrape interval where
applicable.

## `internal/chart` (SVG)

`Render(series ChartSeries) ([]byte, error)` produces one line chart as SVG via
`text/template`. Constraints: no scripts, no external references, inline
presentation only (GitLab sanitizes uploaded SVG); fixed viewBox; axis labels and
units formatted by the caller (bytes → MiB/GiB, cores, bytes/s). Golden-file
tested.

## `internal/gitlab` additions

```go
CurrentUser(ctx) (userID int64, err error)                     // whoami, loop/author gate
MergeRequestDiscussion(ctx, projectID, mrIID, discussionID) (Discussion, error) // root note author + body
UploadFile(ctx, projectID, filename string, content []byte) (markdown string, err error) // POST /uploads
CreateDiscussionReply(ctx, projectID, mrIID, discussionID, body string) error   // reply in-thread
// allowlist source: the existing PipelineJobs(ctx, projectID, pipelineID) is reused (find job by name in Go).
```

`UpsertNote` keeps its signature but its lookup switches to the **marker prefix**
(see below).

## `internal/report` marker change

- `MarkerPrefix = "<!-- ci-resources-bot"` — stable; `UpsertNote` finds the
  existing note by this prefix (preserving idempotency).
- `SignedMarker(pipelineID, mrIID int64, key []byte) string` — renders
  `<!-- ci-resources-bot p=<id> m=<iid> sig=<hex> -->`, embedded in the report
  body. `ParseSignedMarker(line, key)` verifies and returns `(pipelineID, mrIID)`.
- `ProcessPipeline` already has project/pipeline/MR ids to build the signed
  marker; the render path threads the signing key through (serve only — `bot run`
  needs no key and prints without a signed marker).

## Config

- `COMMANDS_ENABLED` (bool, default `false`). When false: `Note Hook` is
  200-ignored and the command path is not wired.
- `COMMANDS_SIGNING_KEY` (string). **Required when `COMMANDS_ENABLED=true`**
  (fail fast in `serve`); must be stable across replicas. Sourced from the chart
  Secret alongside the other secrets. Not required by `bot run`.

## Deployment

- Helm chart: expose `COMMANDS_ENABLED`; add `COMMANDS_SIGNING_KEY` to
  `secrets.existingSecret` / chart-managed Secret. **NetworkPolicy egress is
  unchanged** — SVGs are rendered in-process; uploads and discussion replies go
  to the already-allowed GitLab endpoint; no Grafana/new egress.

## Error handling

- Target not found / not in this report / no metrics → one short reply.
- Any authorization failure (author, HMAC, loop, non-MR) → silent drop.
- Per-command failures are logged (typed zap fields) and never crash the worker.

## Testing

- **command parser** — table-driven: all `details` forms, `job`/`pod` explicit
  and auto-detected, `help`, invalid/ignored lines.
- **authorization** — author gate reject, loop-guard reject, HMAC
  valid/invalid/missing, MR-id mismatch, target outside allowlist reject.
- **chart** — golden SVGs in `testdata/`.
- **metrics range** — `PodSeries`/`PodActiveSpan` verified against a Prometheus
  `query_range` snapshot in `testdata/` (never "by eye").
- **gitlab** — `UploadFile` + `CreateDiscussionReply` + `MergeRequestDiscussion`
  + `CurrentUser` against the mock.
- **webhook** — `Note Hook` with `COMMANDS_ENABLED=false` → 200 ignore, no
  enqueue; enabled + matching command → enqueue; enabled + non-matching body →
  200 ignore; non-MR not.
- **e2e (extended)** — mock GitLab serves discussions/uploads/whoami; mock
  Prometheus serves `query_range`. Drive a `Note Hook` reply → assert one reply
  with 3 embedded uploads. Assert: bot-authored note → no reply (loop guard);
  reply to a non-bot / unsigned root note → no reply; edited marker (bad HMAC) →
  no reply; `details pod` for a pod outside the pipeline → "not part of this
  report" reply.

## Definition of done

- `mise r lint test` clean, race detector on.
- New PromQL (`query_range`) verified against a `testdata/` snapshot.
- Webhook `Note Hook` gating tested (enabled/disabled, matching/non-matching).
- SVG golden files committed; README documents `COMMANDS_ENABLED` /
  `COMMANDS_SIGNING_KEY` and the command syntax (`docs/usage.md`, linked from
  `README.md`).
- Helm chart updated (`helm lint` + `helm template`).

## Out of scope (later)

- Additional commands beyond `help` / `details` (the parser/dispatch is built to
  extend).
- PNG rendering, Grafana links.
- Fuzzy/substring job-name matching (exact match only for now).
