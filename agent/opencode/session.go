package opencode

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
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

// opencodeSession manages multi-turn conversations with the OpenCode CLI.
// Each Send() launches a new `opencode run --format json` process
// with --session for conversation continuity.
type opencodeSession struct {
	cmd               string
	extraArgs         []string // extra args from cmd, prepended before opencode args
	workDir           string
	model             string
	variant           string
	mode              string
	agentName         string
	extraEnv          []string
	events            chan core.Event
	chatID            atomic.Value // stores string — OpenCode session ID
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	alive             atomic.Bool
	expectingContinue atomic.Bool // true when compaction_continue received, waiting for next step
	resultSent        atomic.Bool // true when EventResult has been sent for this turn
	pendingStop       atomic.Bool // true when step_finish reason=stop seen, pending Wait before emitting EventResult
}

func newOpencodeSession(ctx context.Context, cmd string, extraArgs []string, workDir, model, mode, agentName, resumeID string, extraEnv []string, variant string) (*opencodeSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)

	s := &opencodeSession{
		cmd:       cmd,
		extraArgs: extraArgs,
		workDir:   workDir,
		model:     model,
		variant:   variant,
		mode:      mode,
		agentName: agentName,
		extraEnv:  extraEnv,
		events:    make(chan core.Event, 64),
		ctx:       sessionCtx,
		cancel:    cancel,
	}
	s.alive.Store(true)

	if resumeID != "" && resumeID != core.ContinueSession {
		s.chatID.Store(resumeID)
	}

	return s, nil
}

func (s *opencodeSession) Send(prompt string, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if len(files) > 0 {
		filePaths := core.SaveFilesToDisk(s.workDir, messageID, files)
		prompt = core.AppendFileRefs(prompt, filePaths)
	}
	prompt, imagePaths, err := s.stageImages(prompt, images)
	if err != nil {
		return err
	}
	if !s.alive.Load() {
		return fmt.Errorf("session is closed")
	}

	s.resultSent.Store(false)
	s.expectingContinue.Store(false)
	s.pendingStop.Store(false)

	chatID := s.CurrentSessionID()
	isResume := chatID != ""

	args := s.buildRunArgs(prompt, imagePaths, chatID)

	slog.Debug("opencodeSession: launching", "resume", isResume, "args", core.RedactArgs(args))

	cmd := exec.CommandContext(s.ctx, s.cmd, args...)
	cmd.Dir = s.workDir
	env := os.Environ()
	if len(s.extraEnv) > 0 {
		env = core.MergeEnv(env, s.extraEnv)
	}
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("opencodeSession: stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	cmd.Stdin = strings.NewReader(prompt)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opencodeSession: start: %w", err)
	}

	s.wg.Add(1)
	go s.readLoop(cmd, stdout, &stderrBuf)

	return nil
}

func (s *opencodeSession) stageImages(prompt string, images []core.ImageAttachment) (string, []string, error) {
	if len(images) == 0 {
		return prompt, nil, nil
	}

	imgDir := filepath.Join(s.workDir, ".cc-connect", "images")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("opencodeSession: create image dir: %w", err)
	}

	imagePaths := make([]string, 0, len(images))
	for i, img := range images {
		ext := opencodeImageExt(img.MimeType)
		fname := fmt.Sprintf("img_%d_%d%s", time.Now().UnixMilli(), i, ext)
		fpath := filepath.Join(imgDir, fname)
		if err := os.WriteFile(fpath, img.Data, 0o644); err != nil {
			return "", nil, fmt.Errorf("opencodeSession: save image: %w", err)
		}
		imagePaths = append(imagePaths, fpath)
	}

	if prompt == "" {
		prompt = "Please analyze the attached image(s)."
	}

	return prompt, imagePaths, nil
}

func opencodeImageExt(mimeType string) string {
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func (s *opencodeSession) buildRunArgs(prompt string, imagePaths []string, chatID string) []string {
	args := append(append([]string{}, s.extraArgs...), "run", "--format", "json")

	if chatID != "" {
		args = append(args, "--session", chatID)
	}
	if s.agentName != "" {
		args = append(args, "--agent", s.agentName)
	}
	if s.model != "" {
		args = append(args, "--model", s.model)
	}
	if s.workDir != "" {
		args = append(args, "--dir", s.workDir)
	}
	if s.variant != "" {
		args = append(args, "--variant", s.variant)
	}

	// Enable thinking blocks.
	args = append(args, "--thinking")

	// In yolo/auto mode, skip permission prompts entirely so headless
	// runs don't get stuck with auto-rejected external-directory ops.
	if s.mode == "yolo" {
		args = append(args, "--dangerously-skip-permissions")
	}

	for _, imagePath := range imagePaths {
		if imagePath == "" {
			continue
		}
		args = append(args, "--file", imagePath)
	}

	return args
}

// opencodeReadDrainGrace bounds how long readLoop waits for buffered stdout
// to drain after the opencode process itself has already exited (confirmed
// via cmd.Wait(), signaled by waitDone below). It only starts counting once
// the process is known dead, so it can never cut off a turn that is still
// genuinely running — only a stdout pipe stuck open because a detached
// grandchild still holds a handle to it, which Windows cannot cancel by
// closing our end (mirrors agent/codex/session.go's codexReadDrainGrace).
var opencodeReadDrainGrace = 5 * time.Second

func (s *opencodeSession) readLoop(cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf *bytes.Buffer) {
	defer s.wg.Done()
	readDone := make(chan struct{})
	waitErrCh, waitDone := s.startReadLoopWait(cmd, stdout, readDone)

	var stateMu sync.Mutex
	var scanErr error

	// Scanning runs in its own wg-tracked goroutine so this function can stop
	// waiting on it once the process is confirmed dead, without racing
	// Close()'s events-channel shutdown (Close() only closes s.events after
	// s.wg.Wait() succeeds; if wg.Wait() times out instead, close is skipped
	// entirely, so a still-running abandoned goroutine can never send on an
	// already-closed channel).
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(readDone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var raw map[string]any
			if err := json.Unmarshal([]byte(line), &raw); err != nil {
				slog.Debug("opencodeSession: non-JSON line", "line", line)
				continue
			}

			s.handleEvent(raw)
		}
		stateMu.Lock()
		scanErr = scanner.Err()
		stateMu.Unlock()
	}()

	stuck := false
	select {
	case <-readDone:
		// Normal path: stdout hit EOF on its own.
	case <-s.ctx.Done():
	case <-waitDone:
		// The opencode process itself has already exited. Give already-
		// buffered output a short grace window to drain, then stop waiting
		// on the reader instead of blocking forever behind a lingering
		// grandchild's pipe handle.
		select {
		case <-readDone:
		case <-s.ctx.Done():
		case <-time.After(opencodeReadDrainGrace):
			stuck = true
			slog.Warn("opencodeSession: process exited but its output stream is still blocked after grace period; a lingering child process likely still holds the pipe open — reporting best-known turn state and abandoning the stuck reader",
				"grace", opencodeReadDrainGrace)
		}
	}

	stateMu.Lock()
	scanErrSnapshot := scanErr
	stateMu.Unlock()

	if scanErrSnapshot != nil {
		select {
		case <-s.ctx.Done():
			scanErrSnapshot = nil
		case <-waitDone:
			scanErrSnapshot = nil
		default:
			slog.Error("opencodeSession: scanner error", "error", scanErrSnapshot)
		}
	}

	waitErr := <-waitErrCh

	if s.ctx.Err() != nil {
		return
	}

	if stuck {
		// Unlike the normal fallback below, we genuinely do not know whether
		// this turn succeeded — do not let the no-error-seen fallback below
		// mistake this for success.
		slog.Error("opencodeSession: process exited but its final output could not be read before the stuck reader was abandoned")
		evt := core.Event{Type: core.EventError, Error: fmt.Errorf("opencode process exited but its output stream stayed blocked (likely a lingering child process still holds the output pipe open)")}
		select {
		case s.events <- evt:
		case <-s.ctx.Done():
		}
		s.pendingStop.Store(false)
		return
	}

	// Prioritize wait error / scan error over fallback; mirroring codex/claudecode pattern:
	// waitErr and scanErr are terminal, but waitDone/ctx suppression already applied above.
	if scanErrSnapshot != nil {
		evt := core.Event{Type: core.EventError, Error: fmt.Errorf("read stdout: %w", scanErrSnapshot)}
		select {
		case s.events <- evt:
		case <-s.ctx.Done():
		}
		return
	}

	stderrMsg := strings.TrimSpace(stderrBuf.String())
	if stderrMsg != "" {
		// Treat stderr as error only if we haven't already sent a result and no successful turn completion is pending.
		// If expectingContinue, suppress stderr-driven error (compaction continuation is not an error).
		if s.expectingContinue.Load() {
			slog.Info("opencodeSession: readLoop ended after compaction_continue, skipping EventResult despite stderr", "session_id", s.CurrentSessionID(), "stderr", truncate(stderrMsg, 500))
			s.expectingContinue.Store(false)
			return
		}
		if waitErr != nil || !s.resultSent.Load() {
			slog.Error("opencodeSession: process error", "stderr", truncate(stderrMsg, 500), "waitErr", waitErr)
			if strings.Contains(stderrMsg, "Session not found") {
				s.chatID.Store("")
				slog.Warn("opencodeSession: cleared stale session ID")
			}
			evt := core.Event{Type: core.EventError, Error: fmt.Errorf("%s", stderrMsg)}
			select {
			case s.events <- evt:
			case <-s.ctx.Done():
			}
			return
		}
	}
	if waitErr != nil {
		slog.Error("opencodeSession: process failed", "error", waitErr, "stderr", truncate(stderrMsg, 500))
		evt := core.Event{Type: core.EventError, Error: fmt.Errorf("opencode process exited with error: %w", waitErr)}
		select {
		case s.events <- evt:
		case <-s.ctx.Done():
		}
		return
	}

	// Check if we received compaction_continue before readLoop ended.
	// If so, OpenCode will continue with a new turn - do NOT send EventResult.
	// The subsequent process will send its own EventResult when it finishes.
	if s.expectingContinue.Load() {
		slog.Info("opencodeSession: readLoop ended after compaction_continue, skipping EventResult", "session_id", s.CurrentSessionID())
		s.expectingContinue.Store(false)
		s.pendingStop.Store(false)
		return
	}

	// Only send fallback if no terminal event was already emitted (now deferred until after Wait).
	if !s.resultSent.Load() {
		slog.Debug("opencodeSession: readLoop complete, sending fallback EventResult", "session_id", s.CurrentSessionID())
		s.sendEventResult()
	}
	s.pendingStop.Store(false)
}

func (s *opencodeSession) startReadLoopWait(cmd *exec.Cmd, stdout io.ReadCloser, readDone <-chan struct{}) (<-chan error, <-chan struct{}) {
	waitErrCh := make(chan error, 1)
	waitDone := make(chan struct{})

	go func() {
		waitErrCh <- cmd.Wait()
		close(waitDone)
	}()

	go func() {
		select {
		case <-s.ctx.Done():
			_ = stdout.Close()
			return
		case <-waitDone:
		}

		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-readDone:
			return
		case <-timer.C:
		case <-s.ctx.Done():
		}
		_ = stdout.Close()
	}()

	return waitErrCh, waitDone
}

// OpenCode NDJSON event structure:
//
//	{ "type": "text|tool_use|reasoning|step_start|step_finish",
//	  "part": { "type": "text|tool|reasoning|step-start|step-finish", ... } }
func (s *opencodeSession) handleEvent(raw map[string]any) {
	eventType, _ := raw["type"].(string)

	switch eventType {
	case "text":
		s.handleText(raw)
	case "tool_use":
		s.handleToolUse(raw)
	case "reasoning":
		s.handleReasoning(raw)
	case "step_start":
		s.handleStepStart(raw)
	case "step_finish":
		s.handleStepFinish(raw)
	case "error":
		s.handleError(raw)
	default:
		b, _ := json.Marshal(raw)
		slog.Debug("opencodeSession: unhandled event", "type", eventType, "raw", string(b))
	}
}

func (s *opencodeSession) handleText(raw map[string]any) {
	part, _ := raw["part"].(map[string]any)
	if part == nil {
		return
	}
	text, _ := part["text"].(string)

	// Extract metadata and synthetic flags to identify compaction_continue
	metadata, _ := part["metadata"].(map[string]any)
	synthetic, _ := part["synthetic"].(bool)

	// Check for compaction_continue: this is OpenCode's auto-continuation signal.
	// When received, we should NOT send EventText to engine, but mark that we expect
	// a continuation (next step_start will start a new turn without EventResult).
	if synthetic && metadata != nil {
		if cc, ok := metadata["compaction_continue"].(bool); ok && cc {
			slog.Info("opencodeSession: compaction_continue detected, marking expectingContinue", "session_id", s.CurrentSessionID())
			s.expectingContinue.Store(true)
			// Do NOT send EventText - this is internal continuation signal
			return
		}
	}

	if text != "" {
		evt := core.Event{Type: core.EventText, Content: text, Metadata: metadata, Synthetic: synthetic}
		select {
		case s.events <- evt:
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *opencodeSession) handleToolUse(raw map[string]any) {
	part, _ := raw["part"].(map[string]any)
	if part == nil {
		return
	}

	toolName, _ := part["tool"].(string)

	state, _ := part["state"].(map[string]any)
	status := ""
	if state != nil {
		status, _ = state["status"].(string)
	}

	// Extract tool input summary for display
	input := extractToolInput(state)

	if status == "completed" {
		// OpenCode bundles call + result in one event; emit both for UI.
		useEvt := core.Event{Type: core.EventToolUse, ToolName: toolName, ToolInput: input}
		select {
		case s.events <- useEvt:
		case <-s.ctx.Done():
			return
		}

		output, _ := state["output"].(string)
		resultEvt := core.Event{Type: core.EventToolResult, ToolName: toolName, Content: truncate(output, 500)}
		select {
		case s.events <- resultEvt:
		case <-s.ctx.Done():
			return
		}
	} else {
		evt := core.Event{Type: core.EventToolUse, ToolName: toolName, ToolInput: input}
		select {
		case s.events <- evt:
		case <-s.ctx.Done():
			return
		}

		// When a tool call is rejected (e.g. permission denied in default mode),
		// opencode exits without generating any follow-up text. Surface the rejection
		// reason so the engine has something meaningful to send rather than "(空响应)".
		// This covers the common case where the user has not configured tool permissions
		// and needs guidance to use mode="yolo" or update opencode settings.
		if status == "error" && state != nil {
			errMsg, _ := state["error"].(string)
			if errMsg != "" {
				slog.Info("opencodeSession: tool rejected, surfacing error as text", "tool", toolName, "error", errMsg)
				errEvt := core.Event{Type: core.EventText, Content: errMsg}
				select {
				case s.events <- errEvt:
				case <-s.ctx.Done():
					return
				}
			}
		}
	}
}

func extractToolInput(state map[string]any) string {
	if state == nil {
		return ""
	}
	// Prefer title as a concise description (e.g. "List files in current directory")
	if title, ok := state["title"].(string); ok && title != "" {
		return title
	}
	switch input := state["input"].(type) {
	case string:
		return input
	case map[string]any:
		// Use "description" or "command" fields if available
		if desc, ok := input["description"].(string); ok && desc != "" {
			return desc
		}
		if cmd, ok := input["command"].(string); ok && cmd != "" {
			return cmd
		}
		b, _ := json.Marshal(input)
		return truncate(string(b), 200)
	}
	return ""
}

func (s *opencodeSession) handleReasoning(raw map[string]any) {
	part, _ := raw["part"].(map[string]any)
	if part == nil {
		return
	}
	text, _ := part["text"].(string)
	if text != "" {
		evt := core.Event{Type: core.EventThinking, Content: text}
		select {
		case s.events <- evt:
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *opencodeSession) handleError(raw map[string]any) {
	errMsg := extractErrorMessage(raw)
	slog.Error("opencodeSession: agent error", "error", errMsg)
	evt := core.Event{Type: core.EventError, Error: fmt.Errorf("%s", errMsg)}
	select {
	case s.events <- evt:
	case <-s.ctx.Done():
		return
	}
}

// extractErrorMessage tries to pull a human-readable message from various
// OpenCode error JSON shapes.
func extractErrorMessage(raw map[string]any) string {
	// Shape: {"error": {"data": {"message": "..."}, "name": "..."}}
	if errObj, ok := raw["error"].(map[string]any); ok {
		if data, ok := errObj["data"].(map[string]any); ok {
			if msg, ok := data["message"].(string); ok && msg != "" {
				name, _ := errObj["name"].(string)
				if name != "" {
					return name + ": " + msg
				}
				return msg
			}
		}
		if msg, ok := errObj["message"].(string); ok && msg != "" {
			return msg
		}
		if name, ok := errObj["name"].(string); ok && name != "" {
			return name
		}
	}
	// Shape: {"error": "string message"}
	if errStr, ok := raw["error"].(string); ok && errStr != "" {
		return errStr
	}
	// Shape: {"part": {"error": "...", "message": "..."}}
	if part, ok := raw["part"].(map[string]any); ok {
		if msg, ok := part["error"].(string); ok && msg != "" {
			return msg
		}
		if msg, ok := part["message"].(string); ok && msg != "" {
			return msg
		}
	}
	if msg, ok := raw["message"].(string); ok && msg != "" {
		return msg
	}
	b, _ := json.Marshal(raw)
	return string(b)
}

func (s *opencodeSession) handleStepStart(raw map[string]any) {
	sessionID, _ := raw["sessionID"].(string)
	if sessionID == "" {
		part, _ := raw["part"].(map[string]any)
		if part != nil {
			sessionID, _ = part["sessionID"].(string)
		}
	}
	if sessionID != "" {
		s.chatID.Store(sessionID)
		slog.Debug("opencodeSession: session started", "session_id", sessionID)
	}
}

func (s *opencodeSession) handleStepFinish(raw map[string]any) {
	part, _ := raw["part"].(map[string]any)
	reason := ""
	if part != nil {
		reason, _ = part["reason"].(string)
	}
	slog.Debug("opencodeSession: step finished", "reason", reason, "session_id", s.CurrentSessionID())

	if reason == "stop" {
		// Defer EventResult until readLoop's Wait completes (mirrors codex ca5f863).
		// Previously this sent immediately while the child process was still alive → race/stall.
		s.pendingStop.Store(true)
	}
}

func (s *opencodeSession) sendEventResult() {
	if s.resultSent.Load() {
		slog.Debug("opencodeSession: EventResult already sent, skipping", "session_id", s.CurrentSessionID())
		return
	}
	s.resultSent.Store(true)
	sid := s.CurrentSessionID()
	evt := core.Event{Type: core.EventResult, SessionID: sid, Done: true}
	select {
	case s.events <- evt:
	case <-s.ctx.Done():
	}
}

// RespondPermission is a no-op — OpenCode handles permissions internally.
func (s *opencodeSession) RespondPermission(_ string, _ core.PermissionResult) error {
	return nil
}

func (s *opencodeSession) Events() <-chan core.Event {
	return s.events
}

func (s *opencodeSession) CurrentSessionID() string {
	v, _ := s.chatID.Load().(string)
	return v
}

func (s *opencodeSession) Alive() bool {
	return s.alive.Load()
}

func (s *opencodeSession) Close() error {
	s.alive.Store(false)
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		close(s.events)
	case <-time.After(8 * time.Second):
		slog.Warn("opencodeSession: close timed out, abandoning wg.Wait")
	}
	return nil
}

func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "..."
}
