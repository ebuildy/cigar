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
