package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// antigravitySession manages multi-turn conversations with the Antigravity CLI (agy).
type antigravitySession struct {
	cmd              string
	extraArgs        []string // extra args from cmd, prepended before agy args
	workDir          string
	model            string
	mode             string
	timeout          time.Duration
	extraEnv         []string
	events           chan core.Event
	closeOnce        sync.Once
	chatID           atomic.Value // stores string
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	alive            atomic.Bool
	permissionBridge *agyPermissionBridge
}

func newAntigravitySession(ctx context.Context, cmd string, extraArgs []string, workDir, model, mode, resumeID string, extraEnv []string, timeout time.Duration) (*antigravitySession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	as := &antigravitySession{
		cmd:       cmd,
		extraArgs: extraArgs,
		workDir:   workDir,
		model:     model,
		mode:      mode,
		timeout:   timeout,
		extraEnv:  extraEnv,
		events:    make(chan core.Event, 64),
		ctx:       sessionCtx,
		cancel:    cancel,
	}
	as.alive.Store(true)

	if mode == "default" {
		bridge, err := newAgyPermissionBridge(sessionCtx, as.events)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("antigravity: initialize permission bridge: %w", err)
		}
		as.permissionBridge = bridge
	}

	if resumeID != "" && resumeID != core.ContinueSession {
		as.chatID.Store(resumeID)
	}

	return as, nil
}

func (as *antigravitySession) Send(prompt string, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if !as.alive.Load() {
		return fmt.Errorf("session is closed")
	}

	// Save images and files into the workspace
	attachDir := filepath.Join(as.workDir, ".cc-connect", "attachments")
	if (len(images) > 0 || len(files) > 0) && os.MkdirAll(attachDir, 0o755) != nil {
		attachDir = os.TempDir()
	}

	var imageRefs []string
	for i, img := range images {
		ext := ".png"
		switch img.MimeType {
		case "image/jpeg":
			ext = ".jpg"
		case "image/gif":
			ext = ".gif"
		case "image/webp":
			ext = ".webp"
		}
		fname := fmt.Sprintf("img_%d_%d%s", time.Now().UnixMilli(), i, ext)
		fpath := filepath.Join(attachDir, fname)
		if err := os.WriteFile(fpath, img.Data, 0o600); err == nil {
			imageRefs = append(imageRefs, fpath)
		}
	}

	var fileRefs []string
	for i, f := range files {
		fname := filepath.Base(f.FileName)
		if fname == "" || fname == "." || fname == ".." {
			fname = fmt.Sprintf("file_%d_%d", time.Now().UnixMilli(), i)
		}
		fpath := filepath.Join(attachDir, fname)
		if err := os.WriteFile(fpath, f.Data, 0o600); err == nil {
			fileRefs = append(fileRefs, fpath)
		}
	}

	chatID := as.CurrentSessionID()
	isResume := chatID != ""

	// Attach image and file references to prompt
	fullPrompt := prompt
	if len(imageRefs) > 0 {
		if fullPrompt == "" {
			fullPrompt = "Please analyze the attached image(s)."
		}
		fullPrompt += "\n\n[Attached images saved at: " + strings.Join(imageRefs, ", ") + "]"
	}
	if len(fileRefs) > 0 {
		if fullPrompt == "" {
			fullPrompt = "Please analyze the attached file(s)."
		}
		fullPrompt += "\n\n[Attached files saved at: " + strings.Join(fileRefs, ", ") + "]"
	}
	agyConfigDir := ""
	if as.permissionBridge != nil {
		agyConfigDir = as.permissionBridge.AgyConfigDir()
	}
	args := as.buildAntigravityArgs(chatID, isResume, as.mode, agyConfigDir, fullPrompt)
	if strings.TrimSpace(as.model) != "" {
		slog.Warn("antigravitySession: model is configured but ignored because agy does not support --model yet", "model", as.model)
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if as.timeout > 0 {
		ctx, cancel = context.WithTimeout(as.ctx, as.timeout)
	} else {
		ctx, cancel = context.WithCancel(as.ctx)
	}

	started := false
	defer func() {
		if !started {
			cancel()
		}
	}()

	slog.Debug("antigravitySession: launching", "resume", isResume, "args", core.RedactArgs(args))
	cmd := exec.CommandContext(ctx, as.cmd, args...)
	cmd.WaitDelay = 1 * time.Second
	cmd.Dir = as.workDir
	env := os.Environ()
	if len(as.extraEnv) > 0 {
		env = core.MergeEnv(env, as.extraEnv)
	}
	if as.permissionBridge != nil {
		env = core.MergeEnv(env, as.permissionBridge.Env())
	}
	cmd.Env = env

	// Keep stdin disconnected: agy --print consumes piped stdin to EOF before
	// processing the prompt, so an open pipe would deadlock the turn.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("antigravitySession: stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("antigravitySession: start: %w", err)
	}

	started = true
	as.wg.Add(1)
	go func() {
		defer cancel()
		as.readLoop(ctx, cmd, stdout, &stderrBuf, append(imageRefs, fileRefs...))
	}()

	return nil
}

func (as *antigravitySession) buildAntigravityArgs(chatID string, isResume bool, mode, agyConfigDir, fullPrompt string) []string {
	// Prepend extra args from cmd so wrappers like "timeout 3600 agy" work.
	// Keep "-p <prompt>" at the very end because agy consumes the immediate next arg.
	args := append([]string{}, as.extraArgs...)
	if agyConfigDir != "" {
		// Antigravity currently names this compatibility flag --gemini_dir.
		args = append(args, "--gemini_dir="+agyConfigDir, "--print-timeout=24h")
	}
	if isResume {
		args = append(args, "--conversation", chatID)
	}
	switch mode {
	case "yolo":
		args = append(args, "--dangerously-skip-permissions")
	case "plan":
		args = append(args, "--sandbox")
	}
	args = append(args, "--output-format", "stream-json", "-p", fullPrompt)
	return args
}

type agyStreamLine struct {
	Event          string          `json:"event"`
	ConversationID string          `json:"conversation_id"`
	Init           *agyInitEvent   `json:"init,omitempty"`
	StepUpdate     *agyStepUpdate  `json:"step_update,omitempty"`
	Result         *agyResultEvent `json:"result,omitempty"`
}

type agyInitEvent struct {
	Cwd            string   `json:"cwd"`
	Tools          []string `json:"tools"`
	PermissionMode string   `json:"permission_mode"`
}

type agyStepUpdate struct {
	ConversationID string  `json:"conversation_id"`
	StepIndex      int     `json:"step_index"`
	State          string  `json:"state"`
	StepType       string  `json:"step_type"`
	TextDelta      string  `json:"text_delta"`
	DurationSeconds float64 `json:"duration_seconds"`
}

type agyResultEvent struct {
	ConversationID string  `json:"conversation_id"`
	Status         string  `json:"status"`
	Response       string  `json:"response"`
	DurationSeconds float64 `json:"duration_seconds"`
	NumTurns       int     `json:"num_turns"`
}

func parseAgyStreamLine(line []byte) (*agyStreamLine, string, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil, "", false
	}
	if trimmed[0] != '{' {
		return nil, string(line), false
	}
	var ev agyStreamLine
	if err := json.Unmarshal(trimmed, &ev); err != nil || ev.Event == "" {
		return nil, string(line), false
	}
	return &ev, "", true
}

func (as *antigravitySession) handleStreamEvent(ev *agyStreamLine, hasEmittedText *bool) {
	// 1. Capture conversation ID as early as possible.
	cid := ev.ConversationID
	if cid == "" && ev.StepUpdate != nil {
		cid = ev.StepUpdate.ConversationID
	}
	if cid == "" && ev.Result != nil {
		cid = ev.Result.ConversationID
	}
	if cid != "" && as.CurrentSessionID() == "" {
		as.chatID.Store(cid)
		slog.Debug("antigravitySession: detected session ID from stream event", "event", ev.Event, "session_id", cid)
		select {
		case as.events <- core.Event{Type: core.EventText, SessionID: cid}:
		case <-as.ctx.Done():
			return
		}
	}

	// 2. Process event content
	switch ev.Event {
	case "init":
		// Already captured conversation_id above.

	case "step_update":
		if ev.StepUpdate == nil {
			return
		}
		switch ev.StepUpdate.StepType {
		case "agent_response":
			if ev.StepUpdate.TextDelta != "" {
				*hasEmittedText = true
				select {
				case as.events <- core.Event{Type: core.EventText, Content: ev.StepUpdate.TextDelta}:
				case <-as.ctx.Done():
					return
				}
			}
		case "thinking":
			if ev.StepUpdate.TextDelta != "" {
				select {
				case as.events <- core.Event{Type: core.EventThinking, Content: ev.StepUpdate.TextDelta}:
				case <-as.ctx.Done():
					return
				}
			}
		}

	case "result":
		if ev.Result != nil && !*hasEmittedText && ev.Result.Response != "" {
			*hasEmittedText = true
			select {
			case as.events <- core.Event{Type: core.EventText, Content: ev.Result.Response}:
			case <-as.ctx.Done():
				return
			}
		}
	}
}

func (as *antigravitySession) readLoop(ctx context.Context, cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf *bytes.Buffer, tempFiles []string) {
	defer as.wg.Done()
	defer func() {
		for _, f := range tempFiles {
			_ = os.Remove(f)
		}

		err := cmd.Wait()
		sid := as.CurrentSessionID()
		if err != nil {
			stderrMsg := strings.TrimSpace(stderrBuf.String())
			if stderrMsg != "" {
				slog.Error("antigravitySession: process failed", "error", err, "stderr", stderrMsg)
				select {
				case as.events <- core.Event{Type: core.EventError, Error: fmt.Errorf("%s", stderrMsg)}:
				case <-as.ctx.Done():
				}
			}
		}

		// Finalize turn.
		select {
		case as.events <- core.Event{Type: core.EventResult, SessionID: sid, Done: true}:
		case <-as.ctx.Done():
		}
	}()

	go func() {
		<-ctx.Done()
		_ = stdout.Close()
	}()

	reader := bufio.NewReader(stdout)
	var hasEmittedText bool

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			ev, rawText, isJSON := parseAgyStreamLine(line)
			if isJSON && ev != nil {
				as.handleStreamEvent(ev, &hasEmittedText)
			} else if rawText != "" {
				hasEmittedText = true
				select {
				case as.events <- core.Event{Type: core.EventText, Content: rawText}:
				case <-as.ctx.Done():
					return
				}
			}
		}
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "file already closed") {
				slog.Error("antigravitySession: read error", "error", err)
				select {
				case as.events <- core.Event{Type: core.EventError, Error: fmt.Errorf("read stdout: %w", err)}:
				case <-as.ctx.Done():
				}
			}
			return
		}
	}
}

func (as *antigravitySession) RespondPermission(requestID string, result core.PermissionResult) error {
	if !as.alive.Load() {
		return fmt.Errorf("session is closed")
	}
	if as.permissionBridge == nil {
		return fmt.Errorf("antigravity: permission responses are only available in default mode")
	}
	return as.permissionBridge.RespondPermission(requestID, result)
}

func (as *antigravitySession) Events() <-chan core.Event {
	return as.events
}

func (as *antigravitySession) CurrentSessionID() string {
	v, _ := as.chatID.Load().(string)
	return v
}

func (as *antigravitySession) Alive() bool {
	return as.alive.Load()
}

func (as *antigravitySession) Close() error {
	as.alive.Store(false)
	as.cancel()
	if as.permissionBridge != nil {
		as.permissionBridge.Close()
	}
	done := make(chan struct{})
	go func() {
		as.wg.Wait()
		as.closeOnce.Do(func() {
			close(as.events)
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		slog.Warn("antigravitySession: close timed out")
	}
	return nil
}
