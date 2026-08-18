package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeTranscript writes a minimal JSONL transcript for sessionID under
// the given home dir's projects/<encoded-workdir> directory and returns the
// agent configured with that workdir.
func writeFakeTranscript(t *testing.T, homeDir, workDir, sessionID string, lines []string) *Agent {
	t.Helper()
	projectsBase := filepath.Join(homeDir, ".claude", "projects")
	key := encodeClaudeProjectKey(workDir)
	dir := filepath.Join(projectsBase, key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir projects dir: %v", err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	opts := map[string]any{"work_dir": workDir}
	a, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a.(*Agent)
}

func transcriptLine(typ, role, content string) string {
	return `{"type":"` + typ + `","timestamp":"2026-08-18T08:00:00+02:00","message":{"role":"` + role + `","content":` + content + `}}`
}

func TestIsSessionLimitEnding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	// USERPROFILE and HOME must point at the same fake home on Windows.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	workDir := filepath.Join(home, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir work dir: %v", err)
	}
	ctx := context.Background()

	cases := []struct {
		name    string
		lines   []string
		wantHit bool
		wantErr bool
	}{
		{
			name: "exact session limit wall",
			lines: []string{
				transcriptLine("user", "user", `"Standing Participation v1 scheduled check."`),
				transcriptLine("assistant", "assistant", `"You've hit your session limit · resets 10am (Europe/Paris)"`),
			},
			wantHit: true,
		},
		{
			name: "apostrophe variant",
			lines: []string{
				transcriptLine("user", "user", `"hello"`),
				transcriptLine("assistant", "assistant", `"You have hit your session limit"`),
			},
			wantHit: true,
		},
		{
			name: "session limit reached variant",
			lines: []string{
				transcriptLine("user", "user", `"hello"`),
				transcriptLine("assistant", "assistant", `"session limit reached"`),
			},
			wantHit: true,
		},
		{
			name: "real reply mentioning the phrase in a long answer",
			lines: []string{
				transcriptLine("user", "user", `"check mail"`),
				transcriptLine("assistant", "assistant", `"I checked the mailbox. Note: if you hit your session limit later, the work may be lost. Here is the full report with twelve items and all evidence files listed below with their paths and statuses."`),
			},
			wantHit: false,
		},
		{
			name: "normal short reply",
			lines: []string{
				transcriptLine("user", "user", `"hello"`),
				transcriptLine("assistant", "assistant", `"All clear."`),
			},
			wantHit: false,
		},
		{
			name: "single-object content block (real claude shape)",
			lines: []string{
				transcriptLine("user", "user", `"Standing Participation v1 scheduled check."`),
				`{"type":"assistant","timestamp":"2026-08-18T09:52:12+02:00","message":{"role":"assistant","content":{"type":"text","text":"You've hit your session limit · resets 10am (Europe/Paris)"}}}`,
			},
			wantHit: true,
		},
		{
			name: "empty transcript",
			lines:   []string{},
			wantHit: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := "test-session-" + sanitizeTestName(tc.name)
			a := writeFakeTranscript(t, home, workDir, sessionID, tc.lines)
			hit, err := a.IsSessionLimitEnding(ctx, sessionID)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if hit != tc.wantHit {
				t.Errorf("IsSessionLimitEnding = %v, want %v", hit, tc.wantHit)
			}
		})
	}
}

func TestIsSessionLimitEnding_EmptySessionID(t *testing.T) {
	a := &Agent{}
	hit, err := a.IsSessionLimitEnding(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hit {
		t.Fatalf("empty session id must not report a limit hit")
	}
}

func sanitizeTestName(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}
