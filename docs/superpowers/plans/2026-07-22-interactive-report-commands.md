# Interactive Report Commands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let users reply to the bot's MR report comment with commands (`help`, `details [job|pod] <name>`); `details` replies with three SVG charts (CPU, memory, network) of the target's resource usage.

**Architecture:** The existing `webhook → queue → worker → reporter` pipeline is generalized to carry either a pipeline event or a note-command event on the same queue. A new `internal/command` package parses and executes commands. Authorization never trusts note text: it requires the discussion root note to be authored by the bot, verifies an HMAC-signed marker on it, and builds the allowed job/pod set from live GitLab pipeline data scoped to the webhook payload's project. Charts are rendered in-process as sanitizer-safe SVG and uploaded to GitLab.

**Tech Stack:** Go ≥ 1.26, `gofiber/fiber/v3`, `spf13/cobra`, `gitlab.com/gitlab-org/api/client-go`, `prometheus/client_golang`, `go.uber.org/zap`, stdlib `text/template`, `crypto/hmac`.

**Reference spec:** `docs/superpowers/specs/2026-07-22-interactive-report-commands-design.md`

---

## File Structure

**New files**
- `internal/report/marker.go` — signed-marker sign/render/parse.
- `internal/report/marker_test.go`
- `internal/command/command.go` — `Command`, `NoteEvent`, `Parse`, help text.
- `internal/command/command_test.go`
- `internal/command/handler.go` — `Handler.Handle`: authorize + execute.
- `internal/command/handler_test.go`
- `internal/chart/chart.go` — SVG line-chart renderer.
- `internal/chart/chart_test.go`
- `internal/chart/testdata/cpu.svg` — golden.
- `internal/metrics/series.go` — `Point`/`Line`/`PodSeries` types + `SeriesSource`.
- `internal/metrics/prom_series_test.go`
- `internal/metrics/testdata/query_range.json` — range snapshot.

**Modified files**
- `internal/config/config.go` — `CommandsEnabled`, `CommandsSigningKey`.
- `internal/config/config_test.go`
- `internal/report/report.go` — `MarkerPrefix`, `Data.NoteMarker`, Render override.
- `internal/report/report_test.go`
- `internal/metrics/prom.go` — export `PromSource`, add range methods.
- `internal/gitlab/client.go` — extend `Client` interface + `Discussion` type.
- `internal/gitlab/gitlab.go` — implement new client methods.
- `internal/gitlab/gitlab_test.go` — cover new methods.
- `internal/reporter/reporter.go` — `SigningKey`, write signed marker, lookup by prefix.
- `internal/reporter/reporter_test.go` — extend `fakeGitLab` with new stubs.
- `internal/webhook/handler.go` — `Event` union, Note Hook routing, `commandsEnabled`.
- `internal/webhook/handler_test.go` — Note Hook cases.
- `cmd/bot/serve.go` — queue `chan webhook.Event`, worker switch, note processing, validation.
- `cmd/bot/deps.go` — build `command.Handler`, resolve bot user id.
- `internal/e2e/e2e_test.go` — mock discussions/uploads/whoami/query_range + note-command tests.
- Docs/chart (final task).

---

## Task 1: Config — `COMMANDS_ENABLED` + `COMMANDS_SIGNING_KEY`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestLoadCommandsConfig(t *testing.T) {
	t.Run("defaults off with no key", func(t *testing.T) {
		t.Setenv("GITLAB_TOKEN", "tok")
		t.Setenv("PROMETHEUS_URL", "http://prom")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.CommandsEnabled {
			t.Fatal("CommandsEnabled = true, want false by default")
		}
		if cfg.CommandsSigningKey != "" {
			t.Fatalf("CommandsSigningKey = %q, want empty", cfg.CommandsSigningKey)
		}
	})
	t.Run("reads enabled and key", func(t *testing.T) {
		t.Setenv("GITLAB_TOKEN", "tok")
		t.Setenv("PROMETHEUS_URL", "http://prom")
		t.Setenv("COMMANDS_ENABLED", "true")
		t.Setenv("COMMANDS_SIGNING_KEY", "s3cret")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.CommandsEnabled {
			t.Fatal("CommandsEnabled = false, want true")
		}
		if cfg.CommandsSigningKey != "s3cret" {
			t.Fatalf("CommandsSigningKey = %q, want %q", cfg.CommandsSigningKey, "s3cret")
		}
	})
	t.Run("rejects non-boolean", func(t *testing.T) {
		t.Setenv("GITLAB_TOKEN", "tok")
		t.Setenv("PROMETHEUS_URL", "http://prom")
		t.Setenv("COMMANDS_ENABLED", "maybe")
		if _, err := Load(); err == nil {
			t.Fatal("Load succeeded, want error on COMMANDS_ENABLED=maybe")
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoadCommandsConfig -v`
Expected: FAIL — `cfg.CommandsEnabled` / `cfg.CommandsSigningKey` undefined (compile error).

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add fields to `Config`:

```go
	PodResolver         string
	CommandsEnabled     bool
	CommandsSigningKey  string
```

In `Load`, set the key alongside the other `os.Getenv` reads:

```go
		PodResolver:         getenv("POD_RESOLVER", "trace"),
		CommandsSigningKey:  os.Getenv("COMMANDS_SIGNING_KEY"),
```

After the `SCRAPE_INTERVAL` block, add:

```go
	if v := os.Getenv("COMMANDS_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("COMMANDS_ENABLED must be a boolean, got %q", v)
		}
		cfg.CommandsEnabled = b
	}
```

(`COMMANDS_SIGNING_KEY` is validated by `serve` only — like `WEBHOOK_SECRET` — so `bot run` never needs it.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestLoadCommandsConfig -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): COMMANDS_ENABLED and COMMANDS_SIGNING_KEY"
```

---

## Task 2: Report — HMAC-signed marker

**Files:**
- Create: `internal/report/marker.go`
- Test: `internal/report/marker_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/report/marker_test.go`:

```go
package report

import "testing"

func TestSignedMarkerRoundTrip(t *testing.T) {
	key := []byte("test-key")
	body := "some report\n" + SignedMarker(42, 3, key) + "\nmore text"

	pid, mr, ok := ParseSignedMarker(body, key)
	if !ok {
		t.Fatal("ParseSignedMarker ok = false, want true")
	}
	if pid != 42 || mr != 3 {
		t.Fatalf("parsed (pipeline=%d, mr=%d), want (42, 3)", pid, mr)
	}
}

func TestSignedMarkerRejectsTamper(t *testing.T) {
	key := []byte("test-key")
	// Marker signs p=42;m=3 but the body claims p=99.
	good := SignedMarker(42, 3, key)
	tampered := replaceFirst(good, "p=42", "p=99")

	if _, _, ok := ParseSignedMarker(tampered, key); ok {
		t.Fatal("ParseSignedMarker accepted a tampered marker")
	}
}

func TestSignedMarkerRejectsWrongKey(t *testing.T) {
	body := SignedMarker(42, 3, []byte("real-key"))
	if _, _, ok := ParseSignedMarker(body, []byte("other-key")); ok {
		t.Fatal("ParseSignedMarker accepted a marker signed with a different key")
	}
}

func TestParseSignedMarkerNoMarker(t *testing.T) {
	if _, _, ok := ParseSignedMarker("no marker here", []byte("k")); ok {
		t.Fatal("ParseSignedMarker ok = true for body without a marker")
	}
}

func TestMarkerPrefixMatchesBothForms(t *testing.T) {
	// UpsertNote finds the note by MarkerPrefix; it must be a substring of both
	// the plain Marker and the signed marker.
	if !contains(Marker, MarkerPrefix) {
		t.Errorf("MarkerPrefix %q not in Marker %q", MarkerPrefix, Marker)
	}
	if !contains(SignedMarker(1, 1, []byte("k")), MarkerPrefix) {
		t.Error("MarkerPrefix not in SignedMarker output")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
func replaceFirst(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/report/ -run 'Marker' -v`
Expected: FAIL — `SignedMarker`/`ParseSignedMarker`/`MarkerPrefix` undefined.

- [ ] **Step 3: Implement**

Create `internal/report/marker.go`:

```go
package report

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
)

// MarkerPrefix is the stable substring UpsertNote matches to find the bot's
// note (present in both the plain Marker and the signed marker), keeping the
// report idempotent regardless of the signature that follows.
const MarkerPrefix = "<!-- ci-resources-bot"

func markerSignature(pipelineID, mrIID int64, key []byte) string {
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "p=%d;m=%d", pipelineID, mrIID)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignedMarker renders the HMAC-signed marker embedded at the top of the report
// body. It pins the report's pipeline and MR so command replies can trust them.
func SignedMarker(pipelineID, mrIID int64, key []byte) string {
	return fmt.Sprintf("%s p=%d m=%d sig=%s -->",
		MarkerPrefix, pipelineID, mrIID, markerSignature(pipelineID, mrIID, key))
}

var signedMarkerRE = regexp.MustCompile(`<!-- ci-resources-bot p=(\d+) m=(\d+) sig=([0-9a-f]+) -->`)

// ParseSignedMarker finds and verifies a signed marker in body. ok is false when
// no marker is present or its signature does not verify against key (tampered).
func ParseSignedMarker(body string, key []byte) (pipelineID, mrIID int64, ok bool) {
	m := signedMarkerRE.FindStringSubmatch(body)
	if m == nil {
		return 0, 0, false
	}
	pipelineID, _ = strconv.ParseInt(m[1], 10, 64)
	mrIID, _ = strconv.ParseInt(m[2], 10, 64)
	want := markerSignature(pipelineID, mrIID, key)
	if !hmac.Equal([]byte(m[3]), []byte(want)) {
		return 0, 0, false
	}
	return pipelineID, mrIID, true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/report/ -run 'Marker' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/report/marker.go internal/report/marker_test.go
git commit -m "feat(report): HMAC-signed marker (sign, parse, prefix lookup)"
```

---

## Task 3: Report — Render writes the signed marker

**Files:**
- Modify: `internal/report/report.go:26-40` (Data struct), `internal/report/report.go:79-83` (Render)
- Test: `internal/report/report_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go`:

```go
func TestRenderUsesNoteMarkerOverride(t *testing.T) {
	d := Data{PipelineID: 42, Status: "success", NoteMarker: "<!-- ci-resources-bot p=42 m=3 sig=deadbeef -->"}
	body, err := Render(d)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !contains(body, d.NoteMarker) {
		t.Fatalf("body missing the override marker:\n%s", body)
	}
}

func TestRenderDefaultsToPlainMarker(t *testing.T) {
	body, err := Render(Data{PipelineID: 1, Status: "success"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !contains(body, Marker) {
		t.Fatalf("body missing plain Marker:\n%s", body)
	}
}
```

(`contains` is defined in `marker_test.go`, same package.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/report/ -run 'RenderUses|RenderDefaults' -v`
Expected: FAIL — `Data.NoteMarker` undefined.

- [ ] **Step 3: Implement**

In `internal/report/report.go`, add the field to `Data` (after `RanJobs`):

```go
	// NoteMarker, when non-empty, is written at the top of the body instead of
	// the plain Marker. serve sets it to a SignedMarker; `bot run` leaves it
	// empty (no signing key, no commands).
	NoteMarker string
```

In `Render`, replace the opening marker write:

```go
	marker := Marker
	if d.NoteMarker != "" {
		marker = d.NoteMarker
	}
	b.WriteString(marker)
	b.WriteString("\n\n")
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/report/ -v`
Expected: PASS (existing golden tests still pass — default path unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): Render honors a signed-marker override"
```

---

## Task 4: Reporter — sign the note, look up by prefix

**Files:**
- Modify: `internal/reporter/reporter.go:19-25` (struct), `:51-63` (ProcessPipeline)
- Test: `internal/reporter/reporter_test.go`

- [ ] **Step 1: Update the fake, then write the failing test**

In `internal/reporter/reporter_test.go`, change `fakeGitLab.UpsertNote` to record the marker and body, and add the four new `gitlab.Client` methods as stubs (needed once Task 7 widens the interface — add them now so this package keeps compiling):

```go
func (f *fakeGitLab) UpsertNote(_ context.Context, _, mrIID int64, marker, body string) error {
	f.upsertedMR = mrIID
	f.upsertMarker = marker
	f.upsertBody = body
	f.upserts++
	return nil
}

// New Client methods (unused by the reporter path; stubbed for the interface).
func (f *fakeGitLab) CurrentUser(context.Context) (int64, error) { return 0, nil }
func (f *fakeGitLab) MergeRequestDiscussion(context.Context, int64, int64, string) (gitlab.Discussion, error) {
	return gitlab.Discussion{}, nil
}
func (f *fakeGitLab) UploadFile(context.Context, int64, string, []byte) (string, error) { return "", nil }
func (f *fakeGitLab) CreateDiscussionReply(context.Context, int64, int64, string, string) error {
	return nil
}
```

Add the recording fields to the `fakeGitLab` struct:

```go
	upsertedMR   int64
	upsertMarker string
	upsertBody   string
	upserts      int
```

Add the test:

```go
func TestProcessPipelineSignsNote(t *testing.T) {
	gl := &fakeGitLab{jobs: []gitlab.Job{{ID: 1, Stage: "build", Name: "compile"}}}
	r := &Reporter{
		GitLab:     gl,
		Resolver:   &fakeResolver{},
		Metrics:    &fakeSource{},
		SigningKey: []byte("k"),
		Log:        zap.NewNop(),
	}
	posted, err := r.ProcessPipeline(context.Background(), 7, 42, 3, "feature-x", "success")
	if err != nil || !posted {
		t.Fatalf("ProcessPipeline posted=%v err=%v", posted, err)
	}
	if gl.upsertMarker != report.MarkerPrefix {
		t.Fatalf("upsert lookup marker = %q, want MarkerPrefix %q", gl.upsertMarker, report.MarkerPrefix)
	}
	pid, mr, ok := report.ParseSignedMarker(gl.upsertBody, []byte("k"))
	if !ok || pid != 42 || mr != 3 {
		t.Fatalf("body signed marker = (%d,%d,%v), want (42,3,true)", pid, mr, ok)
	}
}

func TestProcessPipelineNoKeyPlainMarker(t *testing.T) {
	gl := &fakeGitLab{jobs: []gitlab.Job{{ID: 1, Name: "compile"}}}
	r := &Reporter{GitLab: gl, Resolver: &fakeResolver{}, Metrics: &fakeSource{}, Log: zap.NewNop()}
	if _, err := r.ProcessPipeline(context.Background(), 7, 42, 3, "feature-x", "success"); err != nil {
		t.Fatalf("ProcessPipeline: %v", err)
	}
	if !strings.Contains(gl.upsertBody, report.Marker) {
		t.Fatalf("body missing plain Marker without a signing key:\n%s", gl.upsertBody)
	}
}
```

Add imports `"strings"` and the report package to the test file:

```go
	"strings"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/report"
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/reporter/ -run ProcessPipelineSigns -v`
Expected: FAIL — `Reporter.SigningKey` undefined / marker still `report.Marker`.

- [ ] **Step 3: Implement**

In `internal/reporter/reporter.go`, add to the `Reporter` struct:

```go
	// SigningKey signs the note marker so command replies can trust the report's
	// pipeline/MR. Empty for `bot run` (no commands).
	SigningKey []byte
```

In `ProcessPipeline`, after `data.Status = status` and before `report.Render`, add:

```go
	if len(r.SigningKey) > 0 {
		data.NoteMarker = report.SignedMarker(pipelineID, mrIID, r.SigningKey)
	}
```

Change the `UpsertNote` call to look up by prefix:

```go
	if err := r.GitLab.UpsertNote(ctx, projectID, mrIID, report.MarkerPrefix, body); err != nil {
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/reporter/ ./internal/report/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reporter/reporter.go internal/reporter/reporter_test.go
git commit -m "feat(reporter): sign the note marker, look up existing note by prefix"
```

---

## Task 5: Command parser

**Files:**
- Create: `internal/command/command.go`
- Test: `internal/command/command_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/command/command_test.go`:

```go
package command

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantOK     bool
		wantKind   Kind
		wantTarget TargetType
		wantName   string
	}{
		{name: "help", body: "help", wantOK: true, wantKind: KindHelp},
		{name: "help case-insensitive", body: "HELP", wantOK: true, wantKind: KindHelp},
		{name: "details explicit job", body: "details job build", wantOK: true, wantKind: KindDetails, wantTarget: TargetJob, wantName: "build"},
		{name: "details explicit pod", body: "details pod runner-x-1-2", wantOK: true, wantKind: KindDetails, wantTarget: TargetPod, wantName: "runner-x-1-2"},
		{name: "details auto job", body: "details compile", wantOK: true, wantKind: KindDetails, wantTarget: TargetJob, wantName: "compile"},
		{name: "details auto pod", body: "details runner-abc-project-7-concurrent-0", wantOK: true, wantKind: KindDetails, wantTarget: TargetPod, wantName: "runner-abc-project-7-concurrent-0"},
		{name: "leading blank line", body: "\n\n  details job build  ", wantOK: true, wantKind: KindDetails, wantTarget: TargetJob, wantName: "build"},
		{name: "empty ignored", body: "", wantOK: false},
		{name: "chatter ignored", body: "thanks bot!", wantOK: false},
		{name: "details without target ignored", body: "details", wantOK: false},
		{name: "extra args ignored", body: "details job build extra", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, ok := Parse(tt.body)
			if ok != tt.wantOK {
				t.Fatalf("Parse(%q) ok = %v, want %v", tt.body, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if cmd.Kind != tt.wantKind || cmd.Target != tt.wantTarget || cmd.Name != tt.wantName {
				t.Fatalf("Parse(%q) = %+v, want kind=%v target=%v name=%q",
					tt.body, cmd, tt.wantKind, tt.wantTarget, tt.wantName)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/command/ -run TestParse -v`
Expected: FAIL — package/symbols undefined.

- [ ] **Step 3: Implement**

Create `internal/command/command.go`:

```go
// Package command parses and executes the interactive commands users post as
// replies to the bot's MR report note (help, details ...). It is wired into the
// webhook worker only when COMMANDS_ENABLED.
package command

import (
	"regexp"
	"strings"
)

// Kind is the command verb.
type Kind int

const (
	KindHelp Kind = iota
	KindDetails
)

// TargetType is the resolved kind of a details target.
type TargetType int

const (
	TargetJob TargetType = iota
	TargetPod
)

// Command is a parsed user command.
type Command struct {
	Kind   Kind
	Target TargetType // meaningful for KindDetails
	Name   string     // details target name
}

// NoteEvent is the subset of a GitLab Note Hook the command path needs. It lives
// here (not in webhook) so webhook can depend on command without a cycle.
type NoteEvent struct {
	ProjectID    int64
	MRIID        int64
	NoteID       int64
	DiscussionID string
	AuthorID     int64
	Body         string
}

var (
	helpRE    = regexp.MustCompile(`(?i)^help$`)
	detailsRE = regexp.MustCompile(`(?i)^details\s+(?:(job|pod)\s+)?(\S+)$`)
	runnerRE  = regexp.MustCompile(`^runner-`)
)

// Parse interprets the first non-empty line of body. ok is false when no known
// command matches (the note is ignored).
func Parse(body string) (Command, bool) {
	line := firstNonEmptyLine(body)
	if helpRE.MatchString(line) {
		return Command{Kind: KindHelp}, true
	}
	if m := detailsRE.FindStringSubmatch(line); m != nil {
		cmd := Command{Kind: KindDetails, Name: m[2]}
		switch strings.ToLower(m[1]) {
		case "job":
			cmd.Target = TargetJob
		case "pod":
			cmd.Target = TargetPod
		default: // auto-detect: runner-* is a pod, else a job
			if runnerRE.MatchString(m[2]) {
				cmd.Target = TargetPod
			} else {
				cmd.Target = TargetJob
			}
		}
		return cmd, true
	}
	return Command{}, false
}

func firstNonEmptyLine(body string) string {
	for _, l := range strings.Split(body, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return ""
}

// HelpText is the reply for the help command. Extend it as commands are added.
const HelpText = "**cigar commands**\n\n" +
	"- `help` — show this message\n" +
	"- `details job <name>` — CPU / memory / network charts for a job in this report\n" +
	"- `details pod <runner-...>` — same, for a runner pod in this report\n" +
	"- `details <name>` — auto-detects job vs pod\n"
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/command/ -run TestParse -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/command/command.go internal/command/command_test.go
git commit -m "feat(command): parse help and details [job|pod] <name>"
```

---

## Task 6: Metrics — range series (`PodSeries`, `PodActiveSpan`)

**Files:**
- Create: `internal/metrics/series.go`
- Modify: `internal/metrics/prom.go:14-30` (export `PromSource`)
- Create: `internal/metrics/testdata/query_range.json`, `internal/metrics/prom_series_test.go`

- [ ] **Step 1: Create the range snapshot fixture**

Create `internal/metrics/testdata/query_range.json`:

```json
{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1752912000,"0.10"],[1752912030,"0.20"],[1752912060,"0.30"]]}]}}
```

- [ ] **Step 2: Write the failing test**

Create `internal/metrics/prom_series_test.go`:

```go
package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// rangeServer serves the query_range snapshot and records the queries it saw.
func rangeServer(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("testdata/query_range.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*seen = append(*seen, r.FormValue("query"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPodSeries(t *testing.T) {
	var seen []string
	srv := rangeServer(t, &seen)
	src, err := NewPromSource(srv.URL, 30*time.Second, zap.NewNop())
	if err != nil {
		t.Fatalf("NewPromSource: %v", err)
	}
	start := time.Unix(1752912000, 0)
	end := start.Add(60 * time.Second)

	s, err := src.PodSeries(context.Background(), "runner-x", start, end)
	if err != nil {
		t.Fatalf("PodSeries: %v", err)
	}
	if len(s.CPU.Points) != 3 {
		t.Fatalf("CPU points = %d, want 3", len(s.CPU.Points))
	}
	if s.CPU.Points[2].V != 0.30 {
		t.Fatalf("last CPU value = %v, want 0.30", s.CPU.Points[2].V)
	}
	// Container-level series must exclude the pause container.
	sawExclude := false
	for _, q := range seen {
		if strings.Contains(q, `container!="POD"`) && strings.Contains(q, "container_cpu_usage_seconds_total") {
			sawExclude = true
		}
	}
	if !sawExclude {
		t.Errorf("CPU query did not exclude the POD container; queries=%v", seen)
	}
}

func TestPodActiveSpan(t *testing.T) {
	var seen []string
	srv := rangeServer(t, &seen)
	src, err := NewPromSource(srv.URL, 30*time.Second, zap.NewNop())
	if err != nil {
		t.Fatalf("NewPromSource: %v", err)
	}
	start, end, ok, err := src.PodActiveSpan(context.Background(), "runner-x")
	if err != nil {
		t.Fatalf("PodActiveSpan: %v", err)
	}
	if !ok {
		t.Fatal("PodActiveSpan ok = false, want true")
	}
	if !start.Equal(time.Unix(1752912000, 0)) || !end.Equal(time.Unix(1752912060, 0)) {
		t.Fatalf("span = [%v,%v], want [1752912000,1752912060]", start.Unix(), end.Unix())
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/metrics/ -run 'PodSeries|PodActiveSpan' -v`
Expected: FAIL — `PodSeries`/`PodActiveSpan` undefined.

- [ ] **Step 4: Add the series types**

Create `internal/metrics/series.go`:

```go
package metrics

import (
	"context"
	"time"
)

// Point is one sample of a time series.
type Point struct {
	T time.Time
	V float64
}

// Line is a labeled time series (one chart line).
type Line struct {
	Label  string
	Points []Point
}

// PodSeries holds the time series backing the three `details` charts.
type PodSeries struct {
	CPU    Line // cores
	Memory Line // bytes (working set)
	NetRx  Line // bytes/s
	NetTx  Line // bytes/s
}

// Empty reports whether no series produced any sample (absent ≠ zero).
func (s PodSeries) Empty() bool {
	return len(s.CPU.Points) == 0 && len(s.Memory.Points) == 0 &&
		len(s.NetRx.Points) == 0 && len(s.NetTx.Points) == 0
}

// SeriesSource is the range-query capability consumed by the command handler;
// tests stub it. *PromSource implements it.
type SeriesSource interface {
	PodSeries(ctx context.Context, pod string, start, end time.Time) (PodSeries, error)
	PodActiveSpan(ctx context.Context, pod string) (start, end time.Time, ok bool, err error)
}
```

- [ ] **Step 5: Export PromSource and add range methods**

In `internal/metrics/prom.go`, rename the concrete type and constructor return:

```go
// NewPromSource returns the Prometheus-backed source. It satisfies both Source
// (aggregation) and SeriesSource (range queries).
func NewPromSource(promURL string, scrapeInterval time.Duration, log *zap.Logger) (*PromSource, error) {
	c, err := api.NewClient(api.Config{Address: promURL})
	if err != nil {
		return nil, fmt.Errorf("create prometheus client: %w", err)
	}
	log.Debug("prometheus metrics source created", zap.String("url", promURL))
	return &PromSource{api: promv1.NewAPI(c), scrape: scrapeInterval, log: log}, nil
}

type PromSource struct {
	api    promv1.API
	scrape time.Duration
	log    *zap.Logger
}
```

Then replace every `func (s *promSource)` receiver in this file with `func (s *PromSource)`.

Append the range methods to `internal/metrics/prom.go`:

```go
// activeSpanLookback bounds how far back PodActiveSpan looks for a pod's series.
const activeSpanLookback = 6 * time.Hour

// PodSeries returns aligned CPU/memory/network series over [start,end] via
// query_range. Absent series yield empty lines (never fabricated as zero).
func (s *PromSource) PodSeries(ctx context.Context, pod string, start, end time.Time) (PodSeries, error) {
	step := seriesStep(end.Sub(start))
	rng := rangeVector(step, s.scrape)
	csel := fmt.Sprintf(`pod=%q,container!="",container!="POD"`, pod)
	psel := fmt.Sprintf(`pod=%q`, pod)

	var out PodSeries
	for _, q := range []struct {
		query string
		line  *Line
		label string
	}{
		{fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{%s}[%s]))`, csel, rng), &out.CPU, "cpu"},
		{fmt.Sprintf(`sum(container_memory_working_set_bytes{%s})`, csel), &out.Memory, "memory"},
		{fmt.Sprintf(`sum(rate(container_network_receive_bytes_total{%s}[%s]))`, psel, rng), &out.NetRx, "rx"},
		{fmt.Sprintf(`sum(rate(container_network_transmit_bytes_total{%s}[%s]))`, psel, rng), &out.NetTx, "tx"},
	} {
		pts, err := s.rangePoints(ctx, q.query, start, end, step)
		if err != nil {
			return PodSeries{}, err
		}
		q.line.Label = q.label
		q.line.Points = pts
	}
	return out, nil
}

// PodActiveSpan returns the min/max sample timestamps of the pod's memory series
// over the lookback window. ok is false when the pod has no samples.
func (s *PromSource) PodActiveSpan(ctx context.Context, pod string) (time.Time, time.Time, bool, error) {
	now := time.Now()
	pts, err := s.rangePoints(ctx,
		fmt.Sprintf(`sum(container_memory_working_set_bytes{pod=%q,container!="",container!="POD"})`, pod),
		now.Add(-activeSpanLookback), now, s.scrape)
	if err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if len(pts) == 0 {
		return time.Time{}, time.Time{}, false, nil
	}
	return pts[0].T, pts[len(pts)-1].T, true, nil
}

// rangePoints runs a query_range and flattens the single matrix stream.
func (s *PromSource) rangePoints(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Point, error) {
	val, _, err := s.api.QueryRange(ctx, query, promv1.Range{Start: start, End: end, Step: step})
	if err != nil {
		return nil, fmt.Errorf("prometheus range query %q: %w", query, err)
	}
	m, ok := val.(model.Matrix)
	if !ok {
		return nil, fmt.Errorf("prometheus range query %q: unexpected type %s", query, val.Type())
	}
	if len(m) == 0 {
		return nil, nil
	}
	pts := make([]Point, 0, len(m[0].Values))
	for _, v := range m[0].Values {
		pts = append(pts, Point{T: v.Timestamp.Time(), V: float64(v.Value)})
	}
	return pts, nil
}

// seriesStep keeps charts to ~120 points, floored at one second.
func seriesStep(window time.Duration) time.Duration {
	step := window / 120
	if step < time.Second {
		step = time.Second
	}
	return step
}

// rangeVector is the [range] inside rate(), at least two scrape intervals so a
// rate always has two samples.
func rangeVector(step, scrape time.Duration) string {
	rv := step
	if rv < 2*scrape {
		rv = 2 * scrape
	}
	return fmt.Sprintf("%dms", rv.Milliseconds())
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/metrics/ -v`
Expected: PASS (existing `PodUsage` tests unaffected — the interface `Source` is unchanged and `*PromSource` still satisfies it).

- [ ] **Step 7: Commit**

```bash
git add internal/metrics/series.go internal/metrics/prom.go internal/metrics/prom_series_test.go internal/metrics/testdata/query_range.json
git commit -m "feat(metrics): PodSeries and PodActiveSpan range queries"
```

---

## Task 7: GitLab client — discussions, uploads, whoami

**Files:**
- Modify: `internal/gitlab/client.go` (interface + `Discussion` type)
- Modify: `internal/gitlab/gitlab.go` (implementations)
- Test: `internal/gitlab/gitlab_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/gitlab/gitlab_test.go` (follow the file's existing httptest mock pattern; if it has a helper that builds a client against a mux, reuse it — otherwise use this self-contained form):

```go
func TestNewClientMethods(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":555,"username":"cigar-bot"}`)
	})
	mux.HandleFunc("GET /api/v4/projects/7/merge_requests/3/discussions/abc",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"id":"abc","notes":[{"id":1,"body":"report body","author":{"id":555}}]}`)
		})
	mux.HandleFunc("POST /api/v4/projects/7/uploads", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"markdown":"![cpu.svg](/uploads/deadbeef/cpu.svg)","url":"/uploads/deadbeef/cpu.svg"}`)
	})
	mux.HandleFunc("POST /api/v4/projects/7/merge_requests/3/discussions/abc/notes",
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":2}`)
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(srv.URL, "tok", zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	uid, err := c.CurrentUser(ctx)
	if err != nil || uid != 555 {
		t.Fatalf("CurrentUser = (%d,%v), want (555,nil)", uid, err)
	}
	d, err := c.MergeRequestDiscussion(ctx, 7, 3, "abc")
	if err != nil {
		t.Fatalf("MergeRequestDiscussion: %v", err)
	}
	if d.RootNoteAuthorID != 555 || d.RootNoteBody != "report body" {
		t.Fatalf("discussion root = (%d,%q), want (555,'report body')", d.RootNoteAuthorID, d.RootNoteBody)
	}
	md, err := c.UploadFile(ctx, 7, "cpu.svg", []byte("<svg/>"))
	if err != nil || md == "" {
		t.Fatalf("UploadFile = (%q,%v)", md, err)
	}
	if err := c.CreateDiscussionReply(ctx, 7, 3, "abc", "hi"); err != nil {
		t.Fatalf("CreateDiscussionReply: %v", err)
	}
}
```

Ensure the test file imports `context`, `fmt`, `net/http`, `net/http/httptest`, `testing`, and `go.uber.org/zap`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitlab/ -run TestNewClientMethods -v`
Expected: FAIL — new methods / `Discussion` undefined.

- [ ] **Step 3: Extend the interface and add the type**

In `internal/gitlab/client.go`, add the `Discussion` type and the methods:

```go
// Discussion is the subset of a GitLab MR discussion the command path needs:
// the identity and body of its root (first) note.
type Discussion struct {
	RootNoteAuthorID int64
	RootNoteBody     string
}
```

Add to the `Client` interface:

```go
	// CurrentUser returns the authenticated (bot) user id, for the author/loop
	// guard on command notes.
	CurrentUser(ctx context.Context) (userID int64, err error)

	// MergeRequestDiscussion fetches a discussion and returns its root note's
	// author and body.
	MergeRequestDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string) (Discussion, error)

	// UploadFile uploads content to the project and returns the markdown that
	// embeds it in a note.
	UploadFile(ctx context.Context, projectID int64, filename string, content []byte) (markdown string, err error)

	// CreateDiscussionReply posts body as a reply in the given MR discussion.
	CreateDiscussionReply(ctx context.Context, projectID, mrIID int64, discussionID, body string) error
```

- [ ] **Step 4: Implement**

Append to `internal/gitlab/gitlab.go` (add `"bytes"` to the imports):

```go
func (a *apiClient) CurrentUser(ctx context.Context) (int64, error) {
	u, _, err := a.c.Users.CurrentUser(gl.WithContext(ctx))
	if err != nil {
		return 0, fmt.Errorf("get current user: %w", err)
	}
	return int64(u.ID), nil
}

func (a *apiClient) MergeRequestDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string) (Discussion, error) {
	d, _, err := a.c.Discussions.GetMergeRequestDiscussion(int(projectID), int(mrIID), discussionID, gl.WithContext(ctx))
	if err != nil {
		return Discussion{}, fmt.Errorf("get discussion %s on MR !%d: %w", discussionID, mrIID, err)
	}
	if len(d.Notes) == 0 {
		return Discussion{}, fmt.Errorf("discussion %s has no notes", discussionID)
	}
	root := d.Notes[0]
	return Discussion{RootNoteAuthorID: int64(root.Author.ID), RootNoteBody: root.Body}, nil
}

func (a *apiClient) UploadFile(ctx context.Context, projectID int64, filename string, content []byte) (string, error) {
	f, _, err := a.c.Projects.UploadFile(int(projectID), bytes.NewReader(content), filename, gl.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("upload %q to project %d: %w", filename, projectID, err)
	}
	return f.Markdown, nil
}

func (a *apiClient) CreateDiscussionReply(ctx context.Context, projectID, mrIID int64, discussionID, body string) error {
	_, _, err := a.c.Discussions.AddMergeRequestDiscussionNote(int(projectID), int(mrIID), discussionID,
		&gl.AddMergeRequestDiscussionNoteOptions{Body: gl.Ptr(body)}, gl.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("reply in discussion %s on MR !%d: %w", discussionID, mrIID, err)
	}
	return nil
}
```

> If the build reports a different signature for any `client-go` method (e.g. `Projects.UploadFile` argument order, or `Discussions.GetMergeRequestDiscussion` id types), run `go doc gitlab.com/gitlab-org/api/client-go.<Service>` and adjust the call to match the vendored version. The behavior and our `Client` signatures stay as written.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/gitlab/ -v`
Expected: PASS.

- [ ] **Step 6: Build the whole module (interface widened)**

Run: `go build ./...`
Expected: success. (The reporter's `fakeGitLab` already got the new stubs in Task 4; any other `Client` implementer must too.)

- [ ] **Step 7: Commit**

```bash
git add internal/gitlab/client.go internal/gitlab/gitlab.go internal/gitlab/gitlab_test.go
git commit -m "feat(gitlab): CurrentUser, MergeRequestDiscussion, UploadFile, CreateDiscussionReply"
```

---

## Task 8: Chart — SVG renderer

**Files:**
- Create: `internal/chart/chart.go`
- Test: `internal/chart/chart_test.go`, `internal/chart/testdata/cpu.svg`

- [ ] **Step 1: Write the failing test**

Create `internal/chart/chart_test.go`:

```go
package chart

import (
	"os"
	"strings"
	"testing"
	"time"
)

func sampleSeries() []Series {
	base := time.Unix(1752912000, 0)
	return []Series{{
		Label: "cpu",
		Points: []Point{
			{X: base, Y: 0.1},
			{X: base.Add(30 * time.Second), Y: 0.2},
			{X: base.Add(60 * time.Second), Y: 0.3},
		},
	}}
}

func TestRenderIsSanitizerSafe(t *testing.T) {
	svg, err := Render("CPU (cores)", sampleSeries())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(svg)
	if !strings.HasPrefix(s, "<svg") {
		t.Fatalf("output is not an <svg>: %.40q", s)
	}
	for _, bad := range []string{"<script", "javascript:", "onload=", "http://", "https://"} {
		if strings.Contains(s, bad) {
			t.Errorf("SVG contains disallowed token %q", bad)
		}
	}
	if !strings.Contains(s, "<polyline") {
		t.Error("SVG has no polyline")
	}
}

func TestRenderEmptySeries(t *testing.T) {
	svg, err := Render("Empty", []Series{{Label: "x"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(string(svg), "<svg") {
		t.Fatal("empty series did not produce an <svg>")
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	svg, err := Render("CPU (cores)", sampleSeries())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want, err := os.ReadFile("testdata/cpu.svg")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(svg) != string(want) {
		t.Errorf("SVG != golden. Regenerate with: go test ./internal/chart -run Golden -update\n--- got ---\n%s", svg)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/chart/ -v`
Expected: FAIL — package/`Render` undefined.

- [ ] **Step 3: Implement the renderer**

Create `internal/chart/chart.go`:

```go
// Package chart renders a labeled time series as a self-contained, sanitizer-safe
// SVG line chart (no scripts, no external references, inline presentation only)
// so GitLab renders it when embedded in a note via an upload.
package chart

import (
	"bytes"
	"fmt"
	"text/template"
	"time"
)

// Point is one sample.
type Point struct {
	X time.Time
	Y float64
}

// Series is a labeled line.
type Series struct {
	Label  string
	Points []Point
}

const (
	width   = 600
	height  = 200
	padX    = 40
	padY    = 20
	plotW   = width - 2*padX
	plotH   = height - 2*padY
)

// palette are fixed, colorblind-safe line colors (no external refs).
var palette = []string{"#1f77b4", "#d62728", "#2ca02c", "#9467bd"}

type lineVM struct {
	Color  string
	Points string // "x,y x,y ..."
	Label  string
	LabelY int
}

type chartVM struct {
	Width, Height int
	Title         string
	Lines         []lineVM
}

var tmpl = template.Must(template.New("svg").Parse(
	`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {{.Width}} {{.Height}}" width="{{.Width}}" height="{{.Height}}" font-family="sans-serif" font-size="11">` +
		`<rect x="0" y="0" width="{{.Width}}" height="{{.Height}}" fill="#ffffff"/>` +
		`<text x="8" y="14" fill="#111111">{{.Title}}</text>` +
		`{{range .Lines}}<polyline fill="none" stroke="{{.Color}}" stroke-width="1.5" points="{{.Points}}"/>` +
		`<text x="8" y="{{.LabelY}}" fill="{{.Color}}">{{.Label}}</text>{{end}}` +
		`</svg>`))

// Render draws one chart with the given title and one or more series.
func Render(title string, series []Series) ([]byte, error) {
	minX, maxX, minY, maxY := bounds(series)
	vm := chartVM{Width: width, Height: height, Title: title}
	for i, s := range series {
		vm.Lines = append(vm.Lines, lineVM{
			Color:  palette[i%len(palette)],
			Points: project(s.Points, minX, maxX, minY, maxY),
			Label:  s.Label,
			LabelY: 28 + i*14, // legend labels stacked under the title
		})
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vm); err != nil {
		return nil, fmt.Errorf("render svg: %w", err)
	}
	return buf.Bytes(), nil
}

func bounds(series []Series) (minX, maxX, minY, maxY float64) {
	first := true
	for _, s := range series {
		for _, p := range s.Points {
			x := float64(p.X.Unix())
			if first {
				minX, maxX, minY, maxY = x, x, p.Y, p.Y
				first = false
				continue
			}
			minX, maxX = minf(minX, x), maxf(maxX, x)
			minY, maxY = minf(minY, p.Y), maxf(maxY, p.Y)
		}
	}
	if minY == maxY { // flat or single value: give the axis height
		maxY = minY + 1
	}
	if minX == maxX {
		maxX = minX + 1
	}
	return
}

func project(pts []Point, minX, maxX, minY, maxY float64) string {
	var b bytes.Buffer
	for i, p := range pts {
		x := padX + (float64(p.X.Unix())-minX)/(maxX-minX)*plotW
		y := padY + (1-(p.Y-minY)/(maxY-minY))*plotH
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	return b.String()
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
```

- [ ] **Step 4: Generate the golden file**

Run this one-off to write the golden (the renderer is deterministic):

```bash
cat > /tmp/gen_golden.go <<'EOF'
//go:build ignore
package main
import (
	"os"; "time"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/chart"
)
func main() {
	base := time.Unix(1752912000, 0)
	svg, _ := chart.Render("CPU (cores)", []chart.Series{{Label: "cpu", Points: []chart.Point{
		{X: base, Y: 0.1}, {X: base.Add(30 * time.Second), Y: 0.2}, {X: base.Add(60 * time.Second), Y: 0.3}}}})
	_ = os.WriteFile("internal/chart/testdata/cpu.svg", svg, 0o644)
}
EOF
mkdir -p internal/chart/testdata
go run /tmp/gen_golden.go && rm /tmp/gen_golden.go
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/chart/ -v`
Expected: PASS (golden now matches).

- [ ] **Step 6: Commit**

```bash
git add internal/chart/chart.go internal/chart/chart_test.go internal/chart/testdata/cpu.svg
git commit -m "feat(chart): sanitizer-safe SVG line-chart renderer"
```

---

## Task 9: Command handler — authorize + help

**Files:**
- Create: `internal/command/handler.go`
- Test: `internal/command/handler_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/command/handler_test.go` with fakes and the authorization tests:

```go
package command

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/report"
)

type fakeGitLab struct {
	discussion gitlab.Discussion
	jobs       []gitlab.Job
	replies    []string // reply bodies
	uploads    int
}

func (f *fakeGitLab) PipelineJobs(context.Context, int64, int64) ([]gitlab.Job, error) {
	return f.jobs, nil
}
func (f *fakeGitLab) MergeRequestForBranch(context.Context, int64, string) (int64, bool, error) {
	return 0, false, nil
}
func (f *fakeGitLab) UpsertNote(context.Context, int64, int64, string, string) error { return nil }
func (f *fakeGitLab) JobTrace(context.Context, int64, int64) (string, error)         { return "", nil }
func (f *fakeGitLab) CurrentUser(context.Context) (int64, error)                     { return 555, nil }
func (f *fakeGitLab) MergeRequestDiscussion(context.Context, int64, int64, string) (gitlab.Discussion, error) {
	return f.discussion, nil
}
func (f *fakeGitLab) UploadFile(_ context.Context, _ int64, name string, _ []byte) (string, error) {
	f.uploads++
	return "![" + name + "](/uploads/x/" + name + ")", nil
}
func (f *fakeGitLab) CreateDiscussionReply(_ context.Context, _, _ int64, _, body string) error {
	f.replies = append(f.replies, body)
	return nil
}

type fakeResolver struct{ pods map[int64]string }

func (f *fakeResolver) PodForJob(_ context.Context, _, jobID int64, _, _ time.Time) (string, bool, error) {
	p, ok := f.pods[jobID]
	return p, ok, nil
}

type fakeSeries struct {
	series   metrics.PodSeries
	spanOK   bool
	spanS    time.Time
	spanE    time.Time
}

func (f *fakeSeries) PodSeries(context.Context, string, time.Time, time.Time) (metrics.PodSeries, error) {
	return f.series, nil
}
func (f *fakeSeries) PodActiveSpan(context.Context, string) (time.Time, time.Time, bool, error) {
	return f.spanS, f.spanE, f.spanOK, nil
}

const testKey = "test-key"

// signedRoot returns a discussion whose root note is the bot's signed report.
func signedRoot(pipelineID, mrIID int64) gitlab.Discussion {
	return gitlab.Discussion{
		RootNoteAuthorID: 555,
		RootNoteBody:     report.SignedMarker(pipelineID, mrIID, []byte(testKey)),
	}
}

func newHandler(gl *fakeGitLab, res *fakeResolver, se *fakeSeries) *Handler {
	return &Handler{
		GitLab: gl, Resolver: res, Series: se,
		SigningKey: []byte(testKey), BotUserID: 555, Log: zap.NewNop(),
	}
}

func TestHandleHelp(t *testing.T) {
	gl := &fakeGitLab{discussion: signedRoot(42, 3)}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	err := h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "help"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(gl.replies) != 1 || gl.replies[0] != HelpText {
		t.Fatalf("replies = %v, want one HelpText", gl.replies)
	}
}

func TestHandleIgnoresOwnNote(t *testing.T) {
	gl := &fakeGitLab{discussion: signedRoot(42, 3)}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	// AuthorID == BotUserID: loop guard drops it.
	_ = h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 555, Body: "help"})
	if len(gl.replies) != 0 {
		t.Fatalf("replied to the bot's own note; replies=%v", gl.replies)
	}
}

func TestHandleRejectsNonBotRoot(t *testing.T) {
	d := signedRoot(42, 3)
	d.RootNoteAuthorID = 111 // root not authored by the bot
	gl := &fakeGitLab{discussion: d}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	_ = h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "help"})
	if len(gl.replies) != 0 {
		t.Fatal("replied in a thread whose root is not the bot's report")
	}
}

func TestHandleRejectsTamperedMarker(t *testing.T) {
	d := signedRoot(42, 3)
	d.RootNoteBody = "<!-- ci-resources-bot p=42 m=3 sig=deadbeef -->" // bad signature
	gl := &fakeGitLab{discussion: d}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	_ = h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "help"})
	if len(gl.replies) != 0 {
		t.Fatal("acted on a tampered (bad-HMAC) report note")
	}
}

func TestHandleRejectsMRMismatch(t *testing.T) {
	gl := &fakeGitLab{discussion: signedRoot(42, 999)} // marker pins MR 999
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{})
	_ = h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "help"})
	if len(gl.replies) != 0 {
		t.Fatal("acted when the marker's MR did not match the event MR")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/command/ -run 'TestHandle' -v`
Expected: FAIL — `Handler` undefined.

- [ ] **Step 3: Implement the handler skeleton (authorize + help)**

Create `internal/command/handler.go`:

```go
package command

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/correlate"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/gitlab"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/metrics"
	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/report"
)

// Handler authorizes and executes command notes.
type Handler struct {
	GitLab     gitlab.Client
	Resolver   correlate.Resolver
	Series     metrics.SeriesSource
	SigningKey []byte
	BotUserID  int64
	Log        *zap.Logger
}

// Handle authorizes and runs a single command note. Unauthorized or unparseable
// notes are dropped silently (return nil). It performs at most one reply.
func (h *Handler) Handle(ctx context.Context, ev NoteEvent) error {
	cmd, ok := Parse(ev.Body)
	if !ok {
		return nil
	}
	if ev.AuthorID == h.BotUserID {
		return nil // loop guard: never react to our own notes
	}
	disc, err := h.GitLab.MergeRequestDiscussion(ctx, ev.ProjectID, ev.MRIID, ev.DiscussionID)
	if err != nil {
		return fmt.Errorf("authorize command note %d: %w", ev.NoteID, err)
	}
	if disc.RootNoteAuthorID != h.BotUserID {
		h.Log.Debug("command note not in a bot report thread", zap.Int64("note_id", ev.NoteID))
		return nil
	}
	pipelineID, markerMR, ok := report.ParseSignedMarker(disc.RootNoteBody, h.SigningKey)
	if !ok || markerMR != ev.MRIID {
		h.Log.Warn("command note root marker missing/invalid/mismatched",
			zap.Int64("note_id", ev.NoteID), zap.Bool("marker_ok", ok))
		return nil
	}

	switch cmd.Kind {
	case KindHelp:
		return h.reply(ctx, ev, HelpText)
	case KindDetails:
		return h.details(ctx, ev, pipelineID, cmd)
	}
	return nil
}

func (h *Handler) reply(ctx context.Context, ev NoteEvent, body string) error {
	if err := h.GitLab.CreateDiscussionReply(ctx, ev.ProjectID, ev.MRIID, ev.DiscussionID, body); err != nil {
		return fmt.Errorf("reply to command note %d: %w", ev.NoteID, err)
	}
	return nil
}

// details is implemented in Task 10.
func (h *Handler) details(ctx context.Context, ev NoteEvent, pipelineID int64, cmd Command) error {
	return h.reply(ctx, ev, "details is not implemented yet")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/command/ -run 'TestHandle' -v`
Expected: PASS (help + all four authorization rejections).

- [ ] **Step 5: Commit**

```bash
git add internal/command/handler.go internal/command/handler_test.go
git commit -m "feat(command): handler authorization (author gate, HMAC, loop guard) + help"
```

---

## Task 10: Command handler — `details` execution

**Files:**
- Modify: `internal/command/handler.go` (replace the placeholder `details`, add `resolveTarget`)
- Test: `internal/command/handler_test.go` (add details cases)

- [ ] **Step 1: Write the failing tests**

Add to `internal/command/handler_test.go`:

```go
func nonEmptySeries() metrics.PodSeries {
	base := time.Unix(1752912000, 0)
	pts := []metrics.Point{{T: base, V: 1}, {T: base.Add(time.Minute), V: 2}}
	return metrics.PodSeries{
		CPU:    metrics.Line{Label: "cpu", Points: pts},
		Memory: metrics.Line{Label: "memory", Points: pts},
		NetRx:  metrics.Line{Label: "rx", Points: pts},
		NetTx:  metrics.Line{Label: "tx", Points: pts},
	}
}

func TestHandleDetailsJob(t *testing.T) {
	start := time.Now().Add(-5 * time.Minute)
	end := time.Now()
	gl := &fakeGitLab{
		discussion: signedRoot(42, 3),
		jobs:       []gitlab.Job{{ID: 1, Name: "build", StartedAt: start, FinishedAt: end}},
	}
	se := &fakeSeries{series: nonEmptySeries()}
	res := &fakeResolver{pods: map[int64]string{1: "runner-abc-project-7-concurrent-0"}}
	h := newHandler(gl, res, se)

	err := h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "details job build"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gl.uploads != 3 {
		t.Fatalf("uploads = %d, want 3 (cpu, memory, network)", gl.uploads)
	}
	if len(gl.replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(gl.replies))
	}
}

func TestHandleDetailsPodAllowlist(t *testing.T) {
	start := time.Now().Add(-5 * time.Minute)
	end := time.Now()
	pod := "runner-abc-project-7-concurrent-0"
	gl := &fakeGitLab{
		discussion: signedRoot(42, 3),
		jobs:       []gitlab.Job{{ID: 1, Name: "build", StartedAt: start, FinishedAt: end}},
	}
	se := &fakeSeries{series: nonEmptySeries(), spanOK: true, spanS: start, spanE: end}
	res := &fakeResolver{pods: map[int64]string{1: pod}}
	h := newHandler(gl, res, se)

	// In-allowlist pod: charts posted.
	if err := h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "details pod " + pod}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gl.uploads != 3 {
		t.Fatalf("in-allowlist pod uploads = %d, want 3", gl.uploads)
	}
}

func TestHandleDetailsPodNotInReport(t *testing.T) {
	start := time.Now().Add(-5 * time.Minute)
	end := time.Now()
	gl := &fakeGitLab{
		discussion: signedRoot(42, 3),
		jobs:       []gitlab.Job{{ID: 1, Name: "build", StartedAt: start, FinishedAt: end}},
	}
	se := &fakeSeries{series: nonEmptySeries()}
	res := &fakeResolver{pods: map[int64]string{1: "runner-real-project-7-concurrent-0"}}
	h := newHandler(gl, res, se)

	// A pod NOT correlated to any job of this pipeline must be refused, and no
	// metrics query issued for it.
	if err := h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "details pod runner-evil-project-99-concurrent-0"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gl.uploads != 0 {
		t.Fatalf("uploads = %d, want 0 for a pod outside the report", gl.uploads)
	}
	if len(gl.replies) != 1 {
		t.Fatalf("replies = %d, want 1 (the refusal notice)", len(gl.replies))
	}
}

func TestHandleDetailsUnknownJob(t *testing.T) {
	gl := &fakeGitLab{discussion: signedRoot(42, 3), jobs: []gitlab.Job{{ID: 1, Name: "build"}}}
	h := newHandler(gl, &fakeResolver{}, &fakeSeries{series: nonEmptySeries()})
	if err := h.Handle(context.Background(), NoteEvent{ProjectID: 7, MRIID: 3, DiscussionID: "abc", AuthorID: 9, Body: "details job nope"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if gl.uploads != 0 || len(gl.replies) != 1 {
		t.Fatalf("unknown job: uploads=%d replies=%d, want 0 and 1", gl.uploads, len(gl.replies))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/command/ -run 'TestHandleDetails' -v`
Expected: FAIL — placeholder `details` uploads nothing.

- [ ] **Step 3: Implement `details` and `resolveTarget`**

In `internal/command/handler.go`, add the `chart` import and `strings`/`time`:

```go
	"strings"
	"time"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/chart"
```

Replace the placeholder `details` with:

```go
// details resolves the target against the report's pipeline, queries its series,
// renders three charts, uploads them and posts one reply.
func (h *Handler) details(ctx context.Context, ev NoteEvent, pipelineID int64, cmd Command) error {
	pod, start, end, found, err := h.resolveTarget(ctx, ev.ProjectID, pipelineID, cmd)
	if err != nil {
		return err
	}
	if !found {
		return h.reply(ctx, ev, fmt.Sprintf("`%s` is not part of pipeline #%d's report.", cmd.Name, pipelineID))
	}
	series, err := h.Series.PodSeries(ctx, pod, start, end)
	if err != nil {
		return fmt.Errorf("query series for pod %q: %w", pod, err)
	}
	if series.Empty() {
		return h.reply(ctx, ev, fmt.Sprintf("No metrics found for `%s` in the report window.", cmd.Name))
	}

	charts := []struct {
		file  string
		title string
		lines []chart.Series
	}{
		{"cpu.svg", "CPU (cores)", []chart.Series{toChart(series.CPU)}},
		{"memory.svg", "Memory (bytes)", []chart.Series{toChart(series.Memory)}},
		{"network.svg", "Network (bytes/s)", []chart.Series{toChart(series.NetRx), toChart(series.NetTx)}},
	}
	var body strings.Builder
	fmt.Fprintf(&body, "### Resource usage for `%s`\n\n", cmd.Name)
	for _, c := range charts {
		svg, err := chart.Render(c.title, c.lines)
		if err != nil {
			return fmt.Errorf("render %s: %w", c.file, err)
		}
		md, err := h.GitLab.UploadFile(ctx, ev.ProjectID, c.file, svg)
		if err != nil {
			return fmt.Errorf("upload %s: %w", c.file, err)
		}
		body.WriteString(md)
		body.WriteString("\n\n")
	}
	return h.reply(ctx, ev, body.String())
}

func toChart(l metrics.Line) chart.Series {
	pts := make([]chart.Point, len(l.Points))
	for i, p := range l.Points {
		pts[i] = chart.Point{X: p.T, Y: p.V}
	}
	return chart.Series{Label: l.Label, Points: pts}
}

// resolveTarget validates the target against the pipeline's jobs (the live
// allowlist) and returns the pod plus its chart window. found is false when the
// target is not part of the report.
func (h *Handler) resolveTarget(ctx context.Context, projectID, pipelineID int64, cmd Command) (pod string, start, end time.Time, found bool, err error) {
	jobs, err := h.GitLab.PipelineJobs(ctx, projectID, pipelineID)
	if err != nil {
		return "", time.Time{}, time.Time{}, false, fmt.Errorf("list jobs of pipeline %d: %w", pipelineID, err)
	}
	switch cmd.Target {
	case TargetJob:
		for _, j := range jobs {
			if j.Name != cmd.Name {
				continue
			}
			if j.StartedAt.IsZero() || j.FinishedAt.IsZero() {
				return "", time.Time{}, time.Time{}, false, nil // job never ran
			}
			p, ok, err := h.Resolver.PodForJob(ctx, projectID, j.ID, j.StartedAt, j.FinishedAt)
			if err != nil {
				return "", time.Time{}, time.Time{}, false, err
			}
			if !ok {
				return "", time.Time{}, time.Time{}, false, nil
			}
			return p, j.StartedAt, j.FinishedAt, true, nil
		}
		return "", time.Time{}, time.Time{}, false, nil
	case TargetPod:
		for _, j := range jobs {
			if j.StartedAt.IsZero() || j.FinishedAt.IsZero() {
				continue
			}
			p, ok, err := h.Resolver.PodForJob(ctx, projectID, j.ID, j.StartedAt, j.FinishedAt)
			if err != nil {
				return "", time.Time{}, time.Time{}, false, err
			}
			if !ok || p != cmd.Name {
				continue
			}
			// Pod is in the report. Window from its active span, else the job window.
			s, e, ok, err := h.Series.PodActiveSpan(ctx, p)
			if err != nil {
				return "", time.Time{}, time.Time{}, false, err
			}
			if !ok {
				return p, j.StartedAt, j.FinishedAt, true, nil
			}
			return p, s, e, true, nil
		}
		return "", time.Time{}, time.Time{}, false, nil
	}
	return "", time.Time{}, time.Time{}, false, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/command/ -v`
Expected: PASS (all authorization + details cases).

- [ ] **Step 5: Commit**

```bash
git add internal/command/handler.go internal/command/handler_test.go
git commit -m "feat(command): details execution with live allowlist and 3 SVG charts"
```

---

## Task 11: Webhook — Event union + Note Hook routing

**Files:**
- Modify: `internal/webhook/handler.go`
- Test: `internal/webhook/handler_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/webhook/handler_test.go` (adapt to the file's existing helpers for building the app + a signed/tokened request; the assertions are what matter):

```go
func TestNoteHookDisabledIgnored(t *testing.T) {
	// commandsEnabled=false: a Note Hook is 200-ignored and never enqueued.
	q := &recordingQueue{}
	app := NewApp([]Authenticator{allowAuth{}}, q, zap.NewNop(), false)
	body := `{"object_kind":"note","object_attributes":{"id":1,"note":"help","noteable_type":"MergeRequest","discussion_id":"abc","author_id":9},"project":{"id":7},"merge_request":{"iid":3}}`
	resp := doPost(t, app, "Note Hook", body)
	if resp != 200 {
		t.Fatalf("status = %d, want 200", resp)
	}
	if q.count() != 0 {
		t.Fatalf("enqueued %d, want 0 when commands disabled", q.count())
	}
}

func TestNoteHookMatchingEnqueues(t *testing.T) {
	q := &recordingQueue{}
	app := NewApp([]Authenticator{allowAuth{}}, q, zap.NewNop(), true)
	body := `{"object_kind":"note","object_attributes":{"id":1,"note":"details job build","noteable_type":"MergeRequest","discussion_id":"abc","author_id":9},"project":{"id":7},"merge_request":{"iid":3}}`
	if s := doPost(t, app, "Note Hook", body); s != 200 {
		t.Fatalf("status = %d, want 200", s)
	}
	if q.count() != 1 {
		t.Fatalf("enqueued %d, want 1", q.count())
	}
	ev := q.last()
	if ev.Note == nil || ev.Note.Body != "details job build" || ev.Note.DiscussionID != "abc" || ev.Note.MRIID != 3 {
		t.Fatalf("bad note event: %+v", ev.Note)
	}
}

func TestNoteHookNonCommandIgnored(t *testing.T) {
	q := &recordingQueue{}
	app := NewApp([]Authenticator{allowAuth{}}, q, zap.NewNop(), true)
	body := `{"object_kind":"note","object_attributes":{"id":1,"note":"thanks!","noteable_type":"MergeRequest","discussion_id":"abc","author_id":9},"project":{"id":7},"merge_request":{"iid":3}}`
	if s := doPost(t, app, "Note Hook", body); s != 200 {
		t.Fatalf("status = %d, want 200", s)
	}
	if q.count() != 0 {
		t.Fatalf("enqueued %d, want 0 for a non-command note", q.count())
	}
}

func TestNoteHookNonMRIgnored(t *testing.T) {
	q := &recordingQueue{}
	app := NewApp([]Authenticator{allowAuth{}}, q, zap.NewNop(), true)
	body := `{"object_kind":"note","object_attributes":{"id":1,"note":"help","noteable_type":"Issue","discussion_id":"abc","author_id":9},"project":{"id":7}}`
	if s := doPost(t, app, "Note Hook", body); s != 200 {
		t.Fatalf("status = %d, want 200", s)
	}
	if q.count() != 0 {
		t.Fatalf("enqueued %d, want 0 for a non-MR note", q.count())
	}
}
```

Add these test helpers to `handler_test.go` (or fold into existing equivalents):

```go
type recordingQueue struct {
	mu     sync.Mutex
	events []Event
}

func (q *recordingQueue) Enqueue(ev Event) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.events = append(q.events, ev)
	return true
}
func (q *recordingQueue) count() int { q.mu.Lock(); defer q.mu.Unlock(); return len(q.events) }
func (q *recordingQueue) last() Event { q.mu.Lock(); defer q.mu.Unlock(); return q.events[len(q.events)-1] }

// allowAuth authenticates everything (auth is covered by auth_test.go).
type allowAuth struct{}

func (allowAuth) Authenticate(fiber.Ctx) bool { return true }

func doPost(t *testing.T, app *fiber.App, event, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("X-Gitlab-Event", event)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}
```

Ensure imports include `sync`, `net/http`, `net/http/httptest`, `strings`, `github.com/gofiber/fiber/v3`, `go.uber.org/zap`. If `handler_test.go` already has some of these helpers under different names, reuse those instead of redefining.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/webhook/ -run TestNoteHook -v`
Expected: FAIL — `Event`, `NewApp` arity, `Enqueue(Event)` mismatch (compile errors).

- [ ] **Step 3: Implement the Event union and Note Hook routing**

Rewrite `internal/webhook/handler.go`. Change the imports to add the command package:

```go
import (
	"encoding/json"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"

	"gitlab.com/ebuildy/gitlab-ci-resources-bot/internal/command"
)
```

Add the `Event` union and change `Enqueuer`:

```go
// Event is a unit of work for the worker: exactly one field is non-nil.
type Event struct {
	Pipeline *PipelineEvent
	Note     *command.NoteEvent
}

// Enqueuer hands validated work to the worker. Must not block; false = full.
type Enqueuer interface {
	Enqueue(ev Event) bool
}
```

Add the Note Hook payload type:

```go
type notePayload struct {
	ObjectAttributes struct {
		ID           int64  `json:"id"`
		Note         string `json:"note"`
		NoteableType string `json:"noteable_type"`
		DiscussionID string `json:"discussion_id"`
		AuthorID     int64  `json:"author_id"`
	} `json:"object_attributes"`
	Project struct {
		ID int64 `json:"id"`
	} `json:"project"`
	MergeRequest *struct {
		IID int64 `json:"iid"`
	} `json:"merge_request"`
}
```

Change `NewApp` and `handler` to carry `commandsEnabled`:

```go
func NewApp(auths []Authenticator, queue Enqueuer, log *zap.Logger, commandsEnabled bool) *fiber.App {
	app := fiber.New(fiber.Config{
		BodyLimit:    maxBodyBytes,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	})
	h := &handler{auths: auths, queue: queue, log: log, commandsEnabled: commandsEnabled}
	app.Post("/webhook", h.handle)
	return app
}

type handler struct {
	auths           []Authenticator
	queue           Enqueuer
	log             *zap.Logger
	commandsEnabled bool
}
```

Replace `handle` with a router and split the pipeline path out:

```go
func (h *handler) handle(c fiber.Ctx) error {
	if !h.authenticate(c) {
		h.log.Debug("webhook authentication failed", zap.String("event", c.Get("X-Gitlab-Event")))
		return c.SendStatus(fiber.StatusUnauthorized)
	}
	switch c.Get("X-Gitlab-Event") {
	case "Pipeline Hook":
		return h.handlePipeline(c)
	case "Note Hook":
		if !h.commandsEnabled {
			return c.SendStatus(fiber.StatusOK)
		}
		return h.handleNote(c)
	default:
		return c.SendStatus(fiber.StatusOK)
	}
}

func (h *handler) handlePipeline(c fiber.Ctx) error {
	var ev PipelineEvent
	if err := json.Unmarshal(c.Body(), &ev); err != nil {
		h.log.Warn("malformed pipeline payload", zap.Error(err))
		return c.SendStatus(fiber.StatusBadRequest)
	}
	if !terminalStatuses[ev.ObjectAttributes.Status] {
		h.log.Debug("ignoring non-terminal pipeline status",
			zap.Int64("pipeline_id", ev.ObjectAttributes.ID), zap.String("status", ev.ObjectAttributes.Status))
		return c.SendStatus(fiber.StatusOK)
	}
	if !h.queue.Enqueue(Event{Pipeline: &ev}) {
		h.log.Warn("queue full, dropping event",
			zap.Int64("pipeline_id", ev.ObjectAttributes.ID), zap.Int64("project_id", ev.Project.ID))
		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
	h.log.Debug("enqueued pipeline event",
		zap.Int64("pipeline_id", ev.ObjectAttributes.ID), zap.Int64("project_id", ev.Project.ID),
		zap.String("status", ev.ObjectAttributes.Status))
	return c.SendStatus(fiber.StatusOK)
}

func (h *handler) handleNote(c fiber.Ctx) error {
	var p notePayload
	if err := json.Unmarshal(c.Body(), &p); err != nil {
		h.log.Warn("malformed note payload", zap.Error(err))
		return c.SendStatus(fiber.StatusBadRequest)
	}
	// Only MR notes, and only recognized commands, are enqueued (cheap, no I/O).
	if p.ObjectAttributes.NoteableType != "MergeRequest" || p.MergeRequest == nil {
		return c.SendStatus(fiber.StatusOK)
	}
	if _, ok := command.Parse(p.ObjectAttributes.Note); !ok {
		return c.SendStatus(fiber.StatusOK)
	}
	ne := &command.NoteEvent{
		ProjectID:    p.Project.ID,
		MRIID:        p.MergeRequest.IID,
		NoteID:       p.ObjectAttributes.ID,
		DiscussionID: p.ObjectAttributes.DiscussionID,
		AuthorID:     p.ObjectAttributes.AuthorID,
		Body:         p.ObjectAttributes.Note,
	}
	if !h.queue.Enqueue(Event{Note: ne}) {
		h.log.Warn("queue full, dropping note command", zap.Int64("note_id", ne.NoteID), zap.Int64("project_id", ne.ProjectID))
		return c.SendStatus(fiber.StatusServiceUnavailable)
	}
	h.log.Debug("enqueued note command", zap.Int64("note_id", ne.NoteID), zap.Int64("mr_iid", ne.MRIID))
	return c.SendStatus(fiber.StatusOK)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/webhook/ -v`
Expected: PASS. If existing pipeline tests referenced the old `Enqueue(PipelineEvent)` or 3-arg `NewApp`, update those call sites to the new `Event` / 4-arg (`commandsEnabled`) form (mechanical).

- [ ] **Step 5: Commit**

```bash
git add internal/webhook/handler.go internal/webhook/handler_test.go
git commit -m "feat(webhook): Event union and Note Hook routing gated by commandsEnabled"
```

---

## Task 12: Wire serve + deps

**Files:**
- Modify: `cmd/bot/serve.go`
- Modify: `cmd/bot/deps.go`

- [ ] **Step 1: Update deps to build the command handler and pass the signing key**

In `cmd/bot/deps.go`, set the reporter's signing key in `newReporter` (return value construction):

```go
	return &reporter.Reporter{
		GitLab:            gl,
		Resolver:          resolver,
		Metrics:           source,
		ThrottleWarnRatio: cfg.ThrottleWarnRatio,
		SigningKey:        []byte(cfg.CommandsSigningKey),
		Log:               log,
	}, nil
```

Add a builder for the command handler at the end of `cmd/bot/deps.go` (add imports `context`, the `command`, `gitlab`, `correlate`, `metrics` packages as needed):

```go
// newCommandHandler builds the interactive-command handler. It resolves the bot
// user id once (for the author/loop guard) and reuses the Prometheus source's
// range-query capability.
func newCommandHandler(ctx context.Context, cfg *config.Config, log *zap.Logger) (*command.Handler, error) {
	gl, err := gitlab.New(cfg.GitLabURL, cfg.GitLabToken, log)
	if err != nil {
		return nil, err
	}
	botID, err := gl.CurrentUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve bot user: %w", err)
	}
	var resolver correlate.Resolver
	switch cfg.PodResolver {
	case "trace":
		resolver = correlate.NewTraceResolver(gl, log)
	case "prometheus":
		resolver, err = correlate.NewPromResolver(cfg.PrometheusURL, cfg.ScrapeInterval, log)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown pod resolver %q", cfg.PodResolver)
	}
	source, err := metrics.NewPromSource(cfg.PrometheusURL, cfg.ScrapeInterval, log)
	if err != nil {
		return nil, err
	}
	return &command.Handler{
		GitLab:     gl,
		Resolver:   resolver,
		Series:     source,
		SigningKey: []byte(cfg.CommandsSigningKey),
		BotUserID:  botID,
		Log:        log,
	}, nil
}
```

- [ ] **Step 2: Update serve — validation, queue, worker switch, note dedup**

In `cmd/bot/serve.go`:

Change the queue type and `Enqueue`:

```go
type queue chan webhook.Event

func (q queue) Enqueue(ev webhook.Event) bool {
	select {
	case q <- ev:
		return true
	default:
		return false
	}
}
```

In `serve`, after `rep, err := newReporter(...)`, validate the key and build the handler:

```go
	if cfg.CommandsEnabled && cfg.CommandsSigningKey == "" {
		return errors.New("COMMANDS_ENABLED is true but COMMANDS_SIGNING_KEY is not set")
	}
	var cmdHandler *command.Handler
	if cfg.CommandsEnabled {
		cmdHandler, err = newCommandHandler(ctx, cfg, log)
		if err != nil {
			return err
		}
		log.Info("interactive commands enabled")
	}
```

Change the worker goroutine start and `webhook.NewApp` call:

```go
	q := make(queue, 128)
	go worker(ctx, q, rep, cmdHandler, log)
	log.Debug("worker started")
	...
	app := webhook.NewApp(auths, q, log, cfg.CommandsEnabled)
```

Replace `worker` and add note processing + dedup:

```go
func worker(ctx context.Context, q queue, rep *reporter.Reporter, cmd *command.Handler, log *zap.Logger) {
	seen := make(map[int64]bool) // note IDs already handled (dedup retried deliveries)
	for {
		select {
		case <-ctx.Done():
			log.Debug("worker stopping", zap.Error(ctx.Err()))
			return
		case ev := <-q:
			switch {
			case ev.Pipeline != nil:
				process(ctx, rep, *ev.Pipeline, log)
			case ev.Note != nil && cmd != nil:
				processNote(ctx, cmd, seen, *ev.Note, log)
			}
		}
	}
}

func processNote(ctx context.Context, h *command.Handler, seen map[int64]bool, ev command.NoteEvent, log *zap.Logger) {
	if seen[ev.NoteID] {
		log.Debug("duplicate note delivery ignored", zap.Int64("note_id", ev.NoteID))
		return
	}
	seen[ev.NoteID] = true
	ctx, cancel := context.WithTimeout(ctx, processTimeout)
	defer cancel()
	if err := h.Handle(ctx, ev); err != nil {
		log.Error("handle command note failed", zap.Int64("note_id", ev.NoteID), zap.Error(err))
	}
}
```

Change `process` to take a `PipelineEvent` value (it currently takes `webhook.PipelineEvent`; keep it, just called as `*ev.Pipeline`). Add `"errors"` and the `command` import to `serve.go`.

- [ ] **Step 3: Build and run the full test suite**

Run: `go build ./... && go test ./... 2>&1 | tail -30`
Expected: build succeeds; only the e2e test may fail to compile until Task 13 (its `chanQueue`/worker still use `PipelineEvent`). Everything else passes.

- [ ] **Step 4: Commit**

```bash
git add cmd/bot/serve.go cmd/bot/deps.go
git commit -m "feat(serve): wire command handler, note dedup, generalized queue"
```

---

## Task 13: e2e — note-command chain

**Files:**
- Modify: `internal/e2e/e2e_test.go`

- [ ] **Step 1: Update the harness to the Event queue and add mock endpoints**

In `internal/e2e/e2e_test.go`:

Change `chanQueue` and the harness worker to the new `Event` shape, and make the harness build+run a `command.Handler` alongside the reporter. Update `harness` to also return the handler wiring, and add a `commandsEnabled` parameter to `webhook.NewApp` (pass `true`). Add these mock GitLab endpoints (bot user id `555`, a signed report note as the discussion root):

```go
// GET /user — whoami for the author/loop guard.
mux.HandleFunc("GET /api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprint(w, `{"id":555,"username":"cigar-bot"}`)
})
// GET discussion — root note is the bot's signed report.
mux.HandleFunc(fmt.Sprintf("GET /api/v4/projects/%d/merge_requests/%d/discussions/{id}", projectID, mrIID),
	func(w http.ResponseWriter, _ *http.Request) {
		marker := report.SignedMarker(pipelineID, mrIID, []byte(commandsKey))
		_, _ = fmt.Fprintf(w, `{"id":"disc1","notes":[{"id":1,"body":%q,"author":{"id":555}}]}`, marker)
	})
// POST uploads.
mux.HandleFunc(fmt.Sprintf("POST /api/v4/projects/%d/uploads", projectID),
	func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock(); m.uploads++; m.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"markdown":"![c](/uploads/x/c.svg)","url":"/uploads/x/c.svg"}`)
	})
// POST discussion reply.
mux.HandleFunc(fmt.Sprintf("POST /api/v4/projects/%d/merge_requests/%d/discussions/{id}/notes", projectID, mrIID),
	func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock(); m.replies = append(m.replies, noteBody(t, r)); m.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, `{"id":99}`)
	})
```

Add `uploads int` and `replies []string` to `mockGitLab`, and a `const commandsKey = "e2e-commands-key"`.

Extend `mockProm` to also serve `/api/v1/query_range` (return the same matrix fixture inline):

```go
if strings.HasSuffix(r.URL.Path, "/api/v1/query_range") {
	m.mu.Lock(); m.queries = append(m.queries, r.FormValue("query")); m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1752912000,"1"],[1752912030,"2"]]}]}}`)
	return
}
```

Update `harness` worker loop to dispatch both event kinds (mirroring Task 12), constructing a `command.Handler` with `SigningKey: []byte(commandsKey)`, `BotUserID: 555`, `Series: source`.

- [ ] **Step 2: Add the note-command test and a delivery helper**

```go
func postNoteWebhook(t *testing.T, app *fiber.App, payload string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload))
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	const msgID = "e2e-note"
	mac := hmac.New(sha256.New, []byte(signingKeyRaw))
	mac.Write([]byte(msgID + "." + ts + "." + payload))
	req.Header.Set("webhook-id", msgID)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Gitlab-Event", "Note Hook")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("deliver note webhook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("note webhook status = %d, want 200", resp.StatusCode)
	}
}

func TestNoteCommandDetailsJob(t *testing.T) {
	app, glMock, _ := harness(t, "trace")
	payload := fmt.Sprintf(`{
		"object_kind":"note",
		"object_attributes":{"id":77,"note":"details job build","noteable_type":"MergeRequest","discussion_id":"disc1","author_id":9},
		"project":{"id":%d},
		"merge_request":{"iid":%d}
	}`, projectID, mrIID)

	postNoteWebhook(t, app, payload)
	waitFor(t, "command reply posted", func() bool {
		glMock.mu.Lock()
		defer glMock.mu.Unlock()
		return len(glMock.replies) == 1
	})
	glMock.mu.Lock()
	defer glMock.mu.Unlock()
	if glMock.uploads != 3 {
		t.Fatalf("uploads = %d, want 3", glMock.uploads)
	}
}

func TestNoteCommandLoopGuard(t *testing.T) {
	app, glMock, _ := harness(t, "trace")
	// author_id 555 == bot: must be ignored, no reply.
	payload := fmt.Sprintf(`{
		"object_kind":"note",
		"object_attributes":{"id":78,"note":"help","noteable_type":"MergeRequest","discussion_id":"disc1","author_id":555},
		"project":{"id":%d},"merge_request":{"iid":%d}
	}`, projectID, mrIID)
	postNoteWebhook(t, app, payload)
	// Give the worker a moment; there must be no reply.
	time.Sleep(200 * time.Millisecond)
	glMock.mu.Lock()
	defer glMock.mu.Unlock()
	if len(glMock.replies) != 0 {
		t.Fatalf("replied to the bot's own note; replies=%d", len(glMock.replies))
	}
}
```

- [ ] **Step 3: Run the e2e tests**

Run: `mise r test:e2e`
Expected: PASS — existing pipeline tests plus the new note-command tests.

- [ ] **Step 4: Commit**

```bash
git add internal/e2e/e2e_test.go
git commit -m "test(e2e): note-command chain, uploads, loop guard"
```

---

## Task 14: Docs, chart, and full verification

**Files:**
- Modify: `README.md`, create/modify `docs/usage.md`
- Modify: `deploy/chart/cigar` (values, deployment env, secret keys)

- [ ] **Step 1: Document the feature**

In `docs/usage.md` (create if absent; link from `README.md`), add a "Interactive commands" section: enable with `COMMANDS_ENABLED=true` + `COMMANDS_SIGNING_KEY`; list `help`, `details job <name>`, `details pod <runner-...>`, `details <name>`; note that commands only work as replies within the bot's report thread and are authorized against the report's pipeline. Document the two new env vars in the config table (README) alongside the existing ones.

- [ ] **Step 2: Chart the two env vars into Helm**

In `deploy/chart/cigar`:
- `values.yaml`: add `commands.enabled: false` and wire `COMMANDS_SIGNING_KEY` into `secrets.existingSecret` (and the chart-managed Secret template) next to `WEBHOOK_SECRET`/`GITLAB_TOKEN`.
- Deployment env: map `COMMANDS_ENABLED` from `.Values.commands.enabled` and `COMMANDS_SIGNING_KEY` from the secret.
- NetworkPolicy: unchanged (no new egress).

- [ ] **Step 3: Validate the chart**

Run: `helm lint deploy/chart/cigar && helm template deploy/chart/cigar >/dev/null`
Expected: lint passes; template renders (ignore IDE YAML noise on Go template syntax).

- [ ] **Step 4: Full definition-of-done gate**

Run: `mise r lint test`
Expected: golangci-lint clean; `go test -race ./...` (including e2e) passes.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/usage.md deploy/chart/cigar
git commit -m "docs,chart: document and expose COMMANDS_ENABLED / COMMANDS_SIGNING_KEY"
```

---

## Notes for the implementer

- **client-go signatures:** Task 7 calls `Users.CurrentUser`, `Discussions.GetMergeRequestDiscussion`, `Projects.UploadFile`, `Discussions.AddMergeRequestDiscussionNote`. If the vendored version differs, run `go doc gitlab.com/gitlab-org/api/client-go.<Service>` and adjust the call; keep our `Client` method signatures unchanged.
- **Import-cycle guard:** `command` must NOT import `webhook`. `NoteEvent` lives in `command`; `webhook` imports `command`. Never add a `webhook` import to `command`.
- **`absent ≠ zero`:** `PodSeries` returns empty lines for absent series and the handler replies "no metrics" rather than charting zeros — do not fabricate points.
- **Secrets:** never log `COMMANDS_SIGNING_KEY`, note bodies at info level, or the GitLab token.
- **Golden SVG:** if the renderer changes, regenerate `internal/chart/testdata/cpu.svg` with the Task 8 Step 4 snippet and re-commit.
