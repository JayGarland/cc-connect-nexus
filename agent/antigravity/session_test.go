package antigravity

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"cc-connect", "cc-connect"},
		{"Daily", "daily"},
		{"My Project", "my-project"},
		{"hello_world", "hello-world"},
		{"Test.123", "test-123"},
		{"---weird---", "weird"},
		{"", "project"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := slugify(tt.input)
			if got != tt.want {
				t.Errorf("slugify(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeMode(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"default", "default"},
		{"yolo", "yolo"},
		{"auto", "yolo"},
		{"force", "yolo"},
		{"plan", "plan"},
		{"sandbox", "plan"},
		{"invalid", "default"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := normalizeMode(tt.input)
			if got != tt.want {
				t.Errorf("normalizeMode(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSession_ContinueSessionTreatedAsFresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	s, err := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "", "default", core.ContinueSession, nil, 0)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	if got := s.CurrentSessionID(); got != "" {
		t.Errorf("ContinueSession should be treated as fresh: chatID = %q, want empty", got)
	}
}

func TestBuildAntigravityArgs_PromptAtEnd(t *testing.T) {
	s, _ := newAntigravitySession(context.Background(), "echo", []string{"--verbose"}, "/tmp", "", "default", "", nil, 0)
	args := s.buildAntigravityArgs("sid-1", true, "plan", "/tmp/agy-config", "What is 1+1?")
	if len(args) < 2 {
		t.Fatalf("args too short: %v", args)
	}
	if args[len(args)-2] != "-p" || args[len(args)-1] != "What is 1+1?" {
		t.Fatalf("expected prompt to be final '-p <prompt>', got: %v", args)
	}
	if !contains(args, "--sandbox") {
		t.Fatalf("expected --sandbox in args, got: %v", args)
	}
	if !contains(args, "--gemini_dir=/tmp/agy-config") || !contains(args, "--print-timeout=24h") {
		t.Fatalf("expected isolated Agy config and extended print timeout, got: %v", args)
	}
	if !contains(args, "--verbose") {
		t.Fatalf("expected configured extra args, got: %v", args)
	}
	if contains(args, "-m") || contains(args, "--model") {
		t.Fatalf("did not expect model flags in args, got: %v", args)
	}
}

func TestAntigravitySession_ResumePassesConversationID(t *testing.T) {
	if os.Getenv("GO_WANT_ANTIGRAVITY_HELPER") == "1" {
		argsPath := os.Getenv("CC_ANTIGRAVITY_ARGS_FILE")
		if argsPath == "" {
			os.Exit(2)
		}
		if err := os.WriteFile(argsPath, []byte(strings.Join(os.Args, "\x00")), 0o600); err != nil {
			os.Exit(2)
		}
		_, _ = io.WriteString(os.Stdout, "ok\n")
		os.Exit(0)
	}

	argsPath := t.TempDir() + string(os.PathSeparator) + "args"
	workDir := t.TempDir()
	s, err := newAntigravitySession(
		context.Background(),
		os.Args[0],
		[]string{"-test.run=TestAntigravitySession_ResumePassesConversationID", "--"},
		workDir,
		"",
		"default",
		"conversation-1",
		[]string{
			"GO_WANT_ANTIGRAVITY_HELPER=1",
			"CC_ANTIGRAVITY_ARGS_FILE=" + argsPath,
		},
		0,
	)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Send("second turn", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case event := <-s.Events():
		if event.Type != core.EventText {
			t.Fatalf("first event type = %q, want %q", event.Type, core.EventText)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for helper output")
	}
	select {
	case event := <-s.Events():
		if event.Type != core.EventResult || !event.Done {
			t.Fatalf("completion event = %+v, want done EventResult", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for EventResult")
	}

	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	args := strings.Split(string(data), "\x00")
	if !contains(args, "--conversation") || !contains(args, "conversation-1") {
		t.Fatalf("resume args = %v, want --conversation conversation-1", args)
	}
	if !contains(args, "-p") || !contains(args, "second turn") {
		t.Fatalf("prompt args = %v, want -p second turn", args)
	}
}

func TestDefaultModeCreatesPermissionBridge(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	s, err := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "", "default", "", nil, 0)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	if s.permissionBridge == nil {
		t.Fatal("permissionBridge = nil, want default-mode permission bridge")
	}
	if _, err := os.Stat(filepath.Join(s.permissionBridge.AgyConfigDir(), "config", "hooks.json")); err != nil {
		t.Fatalf("stat Agy hook overlay: %v", err)
	}
}

func TestNonDefaultModesDoNotCreatePermissionBridge(t *testing.T) {
	for _, mode := range []string{"yolo", "plan"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())

			s, err := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "", mode, "", nil, 0)
			if err != nil {
				t.Fatalf("newAntigravitySession: %v", err)
			}
			defer func() { _ = s.Close() }()

			if s.permissionBridge != nil {
				t.Fatalf("permissionBridge = %v, want nil", s.permissionBridge)
			}
		})
	}
}

func TestRespondPermissionRequiresDefaultMode(t *testing.T) {
	s, err := newAntigravitySession(context.Background(), "echo", nil, "/tmp", "", "plan", "", nil, 0)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	err = s.RespondPermission("req", core.PermissionResult{Behavior: "allow"})
	if err == nil || !strings.Contains(err.Error(), "only available in default mode") {
		t.Fatalf("RespondPermission() error = %v, want default-mode error", err)
	}
}

func TestParseAgyStreamLine(t *testing.T) {
	t.Run("init event with conversation_id", func(t *testing.T) {
		raw := []byte(`{"event":"init","conversation_id":"conv-init-123","init":{"cwd":"/tmp","tools":["run_command"]}}` + "\n")
		ev, rawText, isJSON := parseAgyStreamLine(raw)
		if !isJSON || ev == nil {
			t.Fatalf("expected valid JSON event, got isJSON=%v, ev=%v", isJSON, ev)
		}
		if ev.Event != "init" {
			t.Errorf("ev.Event = %q, want 'init'", ev.Event)
		}
		if ev.ConversationID != "conv-init-123" {
			t.Errorf("ev.ConversationID = %q, want 'conv-init-123'", ev.ConversationID)
		}
		if rawText != "" {
			t.Errorf("rawText = %q, want empty", rawText)
		}
	})

	t.Run("step_update agent_response delta", func(t *testing.T) {
		raw := []byte(`{"event":"step_update","step_update":{"conversation_id":"conv-init-123","step_index":2,"state":"ACTIVE","step_type":"agent_response","text_delta":"hello world"}}` + "\n")
		ev, rawText, isJSON := parseAgyStreamLine(raw)
		if !isJSON || ev == nil {
			t.Fatalf("expected valid JSON event, got isJSON=%v, ev=%v", isJSON, ev)
		}
		if ev.Event != "step_update" {
			t.Errorf("ev.Event = %q, want 'step_update'", ev.Event)
		}
		if ev.StepUpdate == nil || ev.StepUpdate.TextDelta != "hello world" {
			t.Errorf("ev.StepUpdate.TextDelta = %v, want 'hello world'", ev.StepUpdate)
		}
		if rawText != "" {
			t.Errorf("rawText = %q, want empty", rawText)
		}
	})

	t.Run("result event", func(t *testing.T) {
		raw := []byte(`{"event":"result","result":{"conversation_id":"conv-init-123","status":"SUCCESS","response":"hello world\n","num_turns":1}}` + "\n")
		ev, rawText, isJSON := parseAgyStreamLine(raw)
		if !isJSON || ev == nil {
			t.Fatalf("expected valid JSON event, got isJSON=%v, ev=%v", isJSON, ev)
		}
		if ev.Event != "result" {
			t.Errorf("ev.Event = %q, want 'result'", ev.Event)
		}
		if ev.Result == nil || ev.Result.Response != "hello world\n" {
			t.Errorf("ev.Result.Response = %v, want 'hello world\\n'", ev.Result)
		}
		if rawText != "" {
			t.Errorf("rawText = %q, want empty", rawText)
		}
	})

	t.Run("raw unstructured text", func(t *testing.T) {
		raw := []byte("plain text output from helper\n")
		ev, rawText, isJSON := parseAgyStreamLine(raw)
		if isJSON || ev != nil {
			t.Fatalf("expected non-JSON, got isJSON=%v, ev=%v", isJSON, ev)
		}
		if rawText != "plain text output from helper\n" {
			t.Errorf("rawText = %q, want 'plain text output from helper\\n'", rawText)
		}
	})

	t.Run("JSON without event field treated as raw", func(t *testing.T) {
		raw := []byte(`{"sessionId":"legacy-123"}` + "\n")
		ev, rawText, isJSON := parseAgyStreamLine(raw)
		if isJSON || ev != nil {
			t.Fatalf("expected non-stream JSON to be treated as rawText, got isJSON=%v, ev=%v", isJSON, ev)
		}
		if rawText != `{"sessionId":"legacy-123"}`+"\n" {
			t.Errorf("rawText = %q", rawText)
		}
	})
}

func TestAntigravitySession_FirstTurnCapturesSessionIDFromStream(t *testing.T) {
	if os.Getenv("GO_WANT_ANTIGRAVITY_STREAM_HELPER") == "1" {
		_, _ = io.WriteString(os.Stdout, `{"event":"init","conversation_id":"stream-captured-uuid-777"}`+"\n")
		_, _ = io.WriteString(os.Stdout, `{"event":"step_update","step_update":{"conversation_id":"stream-captured-uuid-777","step_index":2,"state":"DONE","step_type":"agent_response","text_delta":"streamed token ALPHA-7382"}}`+"\n")
		_, _ = io.WriteString(os.Stdout, `{"event":"result","result":{"conversation_id":"stream-captured-uuid-777","status":"SUCCESS","response":"streamed token ALPHA-7382"}}`+"\n")
		os.Exit(0)
	}

	workDir := t.TempDir()
	s, err := newAntigravitySession(
		context.Background(),
		os.Args[0],
		[]string{"-test.run=TestAntigravitySession_FirstTurnCapturesSessionIDFromStream", "--"},
		workDir,
		"",
		"default",
		"", // fresh turn
		[]string{"GO_WANT_ANTIGRAVITY_STREAM_HELPER=1"},
		0,
	)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Send("first turn", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var text strings.Builder
	var resultEvent core.Event
	deadline := time.After(3 * time.Second)

	for {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventText:
				if ev.Content != "" {
					text.WriteString(ev.Content)
				}
			case core.EventResult:
				resultEvent = ev
				goto done
			}
		case <-deadline:
			t.Fatal("timeout waiting for stream turn completion")
		}
	}
done:

	if !strings.Contains(text.String(), "streamed token ALPHA-7382") {
		t.Errorf("accumulated text = %q, want 'streamed token ALPHA-7382'", text.String())
	}
	if s.CurrentSessionID() != "stream-captured-uuid-777" {
		t.Errorf("s.CurrentSessionID() = %q, want 'stream-captured-uuid-777'", s.CurrentSessionID())
	}
	if resultEvent.SessionID != "stream-captured-uuid-777" {
		t.Errorf("EventResult.SessionID = %q, want 'stream-captured-uuid-777'", resultEvent.SessionID)
	}
}

func TestSendDoesNotHoldStdinOpen(t *testing.T) {
	if os.Getenv("GO_WANT_ANTIGRAVITY_STDIN_HELPER") == "1" {
		_, _ = io.ReadAll(os.Stdin)
		_, _ = io.WriteString(os.Stdout, "done\n")
		os.Exit(0)
	}

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	workDir := t.TempDir()
	s, err := newAntigravitySession(
		context.Background(),
		os.Args[0],
		[]string{"-test.run=TestSendDoesNotHoldStdinOpen", "--"},
		workDir,
		"",
		"default",
		"",
		[]string{"GO_WANT_ANTIGRAVITY_STDIN_HELPER=1"},
		2*time.Second,
	)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Send("hello", "", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	deadline := time.After(3 * time.Second)
	var text strings.Builder
	for {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventPermissionRequest:
				t.Fatal("unexpected permission request from unstructured stdout")
			case core.EventText:
				text.WriteString(ev.Content)
			case core.EventResult:
				if !strings.Contains(text.String(), "done") {
					t.Fatalf("text = %q, want done", text.String())
				}
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for agy process to receive stdin EOF")
		}
	}
}

func TestAntigravitySession_TwoTurnResumeIntegration(t *testing.T) {
	if os.Getenv("GO_WANT_ANTIGRAVITY_TWOTURN_HELPER") == "1" {
		argsPath := os.Getenv("CC_ANTIGRAVITY_ARGS_FILE")
		if argsPath != "" {
			_ = os.WriteFile(argsPath, []byte(strings.Join(os.Args, "\x00")), 0o600)
		}
		// If resuming turn 2
		if contains(os.Args, "--conversation") {
			_, _ = io.WriteString(os.Stdout, `{"event":"init","conversation_id":"conv-two-turn-42"}`+"\n")
			_, _ = io.WriteString(os.Stdout, `{"event":"step_update","step_update":{"conversation_id":"conv-two-turn-42","step_index":2,"state":"DONE","step_type":"agent_response","text_delta":"The remembered token is ALPHA-7382"}}`+"\n")
			_, _ = io.WriteString(os.Stdout, `{"event":"result","result":{"conversation_id":"conv-two-turn-42","status":"SUCCESS","response":"The remembered token is ALPHA-7382"}}`+"\n")
			os.Exit(0)
		}
		// Turn 1 fresh
		_, _ = io.WriteString(os.Stdout, `{"event":"init","conversation_id":"conv-two-turn-42"}`+"\n")
		_, _ = io.WriteString(os.Stdout, `{"event":"step_update","step_update":{"conversation_id":"conv-two-turn-42","step_index":2,"state":"DONE","step_type":"agent_response","text_delta":"ACK ALPHA-7382"}}`+"\n")
		_, _ = io.WriteString(os.Stdout, `{"event":"result","result":{"conversation_id":"conv-two-turn-42","status":"SUCCESS","response":"ACK ALPHA-7382"}}`+"\n")
		os.Exit(0)
	}

	argsPath := t.TempDir() + string(os.PathSeparator) + "args"
	workDir := t.TempDir()
	s, err := newAntigravitySession(
		context.Background(),
		os.Args[0],
		[]string{"-test.run=TestAntigravitySession_TwoTurnResumeIntegration", "--"},
		workDir,
		"",
		"default",
		"", // fresh start
		[]string{
			"GO_WANT_ANTIGRAVITY_TWOTURN_HELPER=1",
			"CC_ANTIGRAVITY_ARGS_FILE=" + argsPath,
		},
		0,
	)
	if err != nil {
		t.Fatalf("newAntigravitySession: %v", err)
	}
	defer func() { _ = s.Close() }()

	// --- Turn 1 ---
	if err := s.Send("remember token ALPHA-7382", "", nil, nil); err != nil {
		t.Fatalf("Send turn 1: %v", err)
	}

	var turn1Text strings.Builder
	var turn1Result core.Event
	deadline := time.After(3 * time.Second)
turn1Loop:
	for {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventText:
				if ev.Content != "" {
					turn1Text.WriteString(ev.Content)
				}
			case core.EventResult:
				turn1Result = ev
				break turn1Loop
			}
		case <-deadline:
			t.Fatal("timeout waiting for turn 1 completion")
		}
	}

	if !strings.Contains(turn1Text.String(), "ACK ALPHA-7382") {
		t.Errorf("turn 1 text = %q, want 'ACK ALPHA-7382'", turn1Text.String())
	}
	if s.CurrentSessionID() != "conv-two-turn-42" {
		t.Fatalf("s.CurrentSessionID() after turn 1 = %q, want 'conv-two-turn-42'", s.CurrentSessionID())
	}
	if turn1Result.SessionID != "conv-two-turn-42" {
		t.Fatalf("turn 1 Result SessionID = %q, want 'conv-two-turn-42'", turn1Result.SessionID)
	}

	// --- Turn 2 ---
	if err := s.Send("what was the token?", "", nil, nil); err != nil {
		t.Fatalf("Send turn 2: %v", err)
	}

	var turn2Text strings.Builder
	var turn2Result core.Event
	deadline = time.After(3 * time.Second)
turn2Loop:
	for {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventText:
				if ev.Content != "" {
					turn2Text.WriteString(ev.Content)
				}
			case core.EventResult:
				turn2Result = ev
				break turn2Loop
			}
		case <-deadline:
			t.Fatal("timeout waiting for turn 2 completion")
		}
	}

	if !strings.Contains(turn2Text.String(), "ALPHA-7382") {
		t.Errorf("turn 2 text = %q, want 'The remembered token is ALPHA-7382'", turn2Text.String())
	}
	if turn2Result.SessionID != "conv-two-turn-42" {
		t.Errorf("turn 2 Result SessionID = %q, want 'conv-two-turn-42'", turn2Result.SessionID)
	}

	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured args for turn 2: %v", err)
	}
	args := strings.Split(string(data), "\x00")
	if !contains(args, "--conversation") || !contains(args, "conv-two-turn-42") {
		t.Fatalf("turn 2 resume args = %v, want --conversation conv-two-turn-42", args)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if strings.TrimSpace(x) == want {
			return true
		}
	}
	return false
}
