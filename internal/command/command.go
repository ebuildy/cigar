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
