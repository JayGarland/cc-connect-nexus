package claudecode

import "testing"

func TestIsInteractiveQuotaWall(t *testing.T) {
	a := &Agent{}
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "monthly spend limit", text: "You've hit your monthly spend limit · raise it at claude.ai/settings/usage?from=cc_cli_limit_message", want: true},
		{name: "session limit", text: "You've hit your session limit · resets 10am", want: true},
		{name: "ordinary response", text: "I checked the project and found no matching file.", want: false},
		{name: "long response mentioning limit", text: "The report explains the session limit in detail and continues with enough ordinary content to avoid treating a real answer as a quota wall.", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := a.IsInteractiveQuotaWall(tt.text); got != tt.want {
				t.Fatalf("IsInteractiveQuotaWall(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}
