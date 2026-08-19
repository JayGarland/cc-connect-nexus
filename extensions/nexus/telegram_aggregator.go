package nexus

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// DefaultTextBatchWindow is the default quiet window for Telegram text aggregation.
// Can be overridden via the platform option `text_batch_window_ms`.
const DefaultTextBatchWindow = 1000 * time.Millisecond

// textBatchEntry holds accumulated consecutive text messages for a single session.
type textBatchEntry struct {
	sessionKey        string
	userID            string
	userName          string
	chatName          string
	channelKey        string
	replyCtx          any
	platform          string
	messageIDs        []string
	contents          []string
	extras            []string
	userMessageTimeMs int64
	timer             *time.Timer
}

// TelegramAggregator buffers consecutive text messages from the same Telegram session
// within a configurable quiet window, coalescing them into a single core.Message turn.
type TelegramAggregator struct {
	mu          sync.Mutex
	batches     map[string]*textBatchEntry
	window      time.Duration
	nextHandler core.MessageHandler
}

// NewTelegramAggregator creates a new consecutive-text aggregator.
func NewTelegramAggregator(window time.Duration, next core.MessageHandler) *TelegramAggregator {
	if window <= 0 {
		window = DefaultTextBatchWindow
	}
	return &TelegramAggregator{
		batches:     make(map[string]*textBatchEntry),
		window:      window,
		nextHandler: next,
	}
}

// Window returns the effective quiet-window duration.
func (a *TelegramAggregator) Window() time.Duration {
	if a == nil || a.window <= 0 {
		return DefaultTextBatchWindow
	}
	return a.window
}

// HandleMessage intercepts an incoming message. If it is a plain conversational text
// message and aggregation is enabled, it buffers the message into the session's batch.
// Non-text messages, attachments, or slash commands flush any pending text batch first
// and are then dispatched immediately.
func (a *TelegramAggregator) HandleMessage(p core.Platform, msg *core.Message) {
	if a == nil || a.nextHandler == nil {
		return
	}
	if msg == nil {
		return
	}

	// Non-text messages (photos, audio, files, location) or slash commands bypass aggregation.
	// We flush any pending conversational text batch for this session first.
	isSlashCommand := strings.HasPrefix(strings.TrimSpace(msg.Content), "/") || strings.HasPrefix(strings.TrimSpace(msg.Content), "!")
	hasAttachments := len(msg.Images) > 0 || len(msg.Files) > 0 || msg.Audio != nil || msg.Location != nil

	if isSlashCommand || hasAttachments {
		a.FlushSession(p, msg.SessionKey)
		a.nextHandler(p, msg)
		return
	}

	a.mu.Lock()
	if a.batches == nil {
		a.batches = make(map[string]*textBatchEntry)
	}

	var toFlush *textBatchEntry
	if existing, ok := a.batches[msg.SessionKey]; ok {
		// Context mismatch check: different user or channel in the same session
		if existing.userID != msg.UserID || existing.channelKey != msg.ChannelKey {
			if existing.timer != nil {
				existing.timer.Stop()
			}
			delete(a.batches, msg.SessionKey)
			toFlush = existing
		}
	}

	if existing, ok := a.batches[msg.SessionKey]; ok {
		// Append to existing batch and reset the quiet timer
		if existing.timer != nil {
			existing.timer.Stop()
		}
		existing.contents = append(existing.contents, msg.Content)
		existing.messageIDs = append(existing.messageIDs, msg.MessageID)
		if msg.ExtraContent != "" {
			existing.extras = append(existing.extras, msg.ExtraContent)
		}
		if msg.UserMessageTimeMs > existing.userMessageTimeMs {
			existing.userMessageTimeMs = msg.UserMessageTimeMs
		}
		existing.replyCtx = msg.ReplyCtx

		ref := existing
		existing.timer = time.AfterFunc(a.window, func() {
			a.flushEntryByRef(p, msg.SessionKey, ref)
		})
	} else {
		// Start a fresh batch with its own quiet-window timer
		var extras []string
		if msg.ExtraContent != "" {
			extras = append(extras, msg.ExtraContent)
		}
		entry := &textBatchEntry{
			sessionKey:        msg.SessionKey,
			userID:            msg.UserID,
			userName:          msg.UserName,
			chatName:          msg.ChatName,
			channelKey:        msg.ChannelKey,
			replyCtx:          msg.ReplyCtx,
			platform:          msg.Platform,
			messageIDs:        []string{msg.MessageID},
			contents:          []string{msg.Content},
			extras:            extras,
			userMessageTimeMs: msg.UserMessageTimeMs,
		}
		ref := entry
		entry.timer = time.AfterFunc(a.window, func() {
			a.flushEntryByRef(p, msg.SessionKey, ref)
		})
		a.batches[msg.SessionKey] = entry
	}
	a.mu.Unlock()

	if toFlush != nil {
		a.dispatchEntry(p, toFlush)
	}
}

// flushEntryByRef dispatches the batch iff the map still points to the same entry pointer.
func (a *TelegramAggregator) flushEntryByRef(p core.Platform, sessionKey string, ref *textBatchEntry) {
	a.mu.Lock()
	current, ok := a.batches[sessionKey]
	if !ok || current != ref {
		a.mu.Unlock()
		return
	}
	if current.timer != nil {
		current.timer.Stop()
	}
	delete(a.batches, sessionKey)
	a.mu.Unlock()

	a.dispatchEntry(p, current)
}

// FlushSession immediately flushes and dispatches any pending batch for a specific session.
func (a *TelegramAggregator) FlushSession(p core.Platform, sessionKey string) {
	a.mu.Lock()
	entry, ok := a.batches[sessionKey]
	if !ok {
		a.mu.Unlock()
		return
	}
	if entry.timer != nil {
		entry.timer.Stop()
	}
	delete(a.batches, sessionKey)
	a.mu.Unlock()

	a.dispatchEntry(p, entry)
}

// Flush synchronously drains and dispatches all pending batches.
// Called on Platform.Stop() so buffered messages are never lost on shutdown.
func (a *TelegramAggregator) Flush(p core.Platform) {
	a.mu.Lock()
	pending := a.batches
	a.batches = make(map[string]*textBatchEntry)
	a.mu.Unlock()

	for _, entry := range pending {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		a.dispatchEntry(p, entry)
	}
}

// dispatchEntry coalesces accumulated text contents and emits a single core.Message.
func (a *TelegramAggregator) dispatchEntry(p core.Platform, entry *textBatchEntry) {
	if entry == nil || len(entry.contents) == 0 {
		return
	}

	canonicalID := entry.messageIDs[len(entry.messageIDs)-1]
	mergedText := strings.Join(entry.contents, "\n\n")
	var extraContent string
	if len(entry.extras) > 0 {
		extraContent = strings.Join(entry.extras, "\n")
	}

	slog.Info("nexus: dispatched aggregated telegram turn",
		"session_key", entry.sessionKey,
		"message_count", len(entry.contents),
		"message_ids", entry.messageIDs,
	)

	mergedMsg := &core.Message{
		SessionKey:        entry.sessionKey,
		Platform:          entry.platform,
		UserID:            entry.userID,
		UserName:          entry.userName,
		ChatName:          entry.chatName,
		ChannelKey:        entry.channelKey,
		MessageID:         canonicalID,
		Content:           mergedText,
		ExtraContent:      extraContent,
		ReplyCtx:          entry.replyCtx,
		UserMessageTimeMs: entry.userMessageTimeMs,
	}

	if a.nextHandler != nil {
		a.nextHandler(p, mergedMsg)
	}
}
