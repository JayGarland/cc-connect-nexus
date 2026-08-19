package nexus

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type mockPlatform struct {
	name string
}

func (m *mockPlatform) Name() string                                                 { return m.name }
func (m *mockPlatform) Start(handler core.MessageHandler) error                      { return nil }
func (m *mockPlatform) Reply(ctx context.Context, replyCtx any, content string) error { return nil }
func (m *mockPlatform) Send(ctx context.Context, replyCtx any, content string) error  { return nil }
func (m *mockPlatform) Stop() error                                                  { return nil }

type capturedCalls struct {
	mu       sync.Mutex
	messages []*core.Message
}

func (c *capturedCalls) Handler(p core.Platform, msg *core.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages = append(c.messages, msg)
}

func (c *capturedCalls) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
}

func (c *capturedCalls) Get(index int) *core.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	if index < 0 || index >= len(c.messages) {
		return nil
	}
	return c.messages[index]
}

// 1. Single message: one input -> one dispatch -> content unchanged.
func TestTelegramAggregator_SingleMessage(t *testing.T) {
	cap := &capturedCalls{}
	p := &mockPlatform{name: "telegram"}
	agg := NewTelegramAggregator(50*time.Millisecond, cap.Handler)

	msg := &core.Message{
		SessionKey: "chat_123",
		Platform:   "telegram",
		UserID:     "user_1",
		UserName:   "Alice",
		MessageID:  "101",
		Content:    "Hello world",
	}

	agg.HandleMessage(p, msg)

	if cap.Count() != 0 {
		t.Fatalf("expected 0 dispatches immediately, got %d", cap.Count())
	}

	// Wait for quiet window
	time.Sleep(80 * time.Millisecond)

	if cap.Count() != 1 {
		t.Fatalf("expected 1 dispatch after quiet window, got %d", cap.Count())
	}

	dispatched := cap.Get(0)
	if dispatched.Content != "Hello world" {
		t.Fatalf("expected content 'Hello world', got %q", dispatched.Content)
	}
	if dispatched.MessageID != "101" {
		t.Fatalf("expected messageID '101', got %q", dispatched.MessageID)
	}
	if dispatched.SessionKey != "chat_123" {
		t.Fatalf("expected sessionKey 'chat_123', got %q", dispatched.SessionKey)
	}
}

// 2. Rapid two/three-message sequence: order preserved, combined once, exactly 1 downstream dispatch.
func TestTelegramAggregator_RapidSequence(t *testing.T) {
	cap := &capturedCalls{}
	p := &mockPlatform{name: "telegram"}
	agg := NewTelegramAggregator(100*time.Millisecond, cap.Handler)

	// Send 3 messages rapidly within the quiet window
	agg.HandleMessage(p, &core.Message{
		SessionKey: "chat_123",
		Platform:   "telegram",
		UserID:     "user_1",
		UserName:   "Alice",
		MessageID:  "101",
		Content:    "Part 1: The quick brown fox",
	})
	time.Sleep(30 * time.Millisecond)

	agg.HandleMessage(p, &core.Message{
		SessionKey: "chat_123",
		Platform:   "telegram",
		UserID:     "user_1",
		UserName:   "Alice",
		MessageID:  "102",
		Content:    "Part 2: jumps over",
	})
	time.Sleep(30 * time.Millisecond)

	agg.HandleMessage(p, &core.Message{
		SessionKey: "chat_123",
		Platform:   "telegram",
		UserID:     "user_1",
		UserName:   "Alice",
		MessageID:  "103",
		Content:    "Part 3: the lazy dog.",
	})

	// Before window expires, must have 0 dispatches
	if cap.Count() != 0 {
		t.Fatalf("expected 0 dispatches while active, got %d", cap.Count())
	}

	// Wait for window to expire
	time.Sleep(140 * time.Millisecond)

	if cap.Count() != 1 {
		t.Fatalf("expected exactly 1 aggregated dispatch, got %d", cap.Count())
	}

	dispatched := cap.Get(0)
	expectedContent := "Part 1: The quick brown fox\n\nPart 2: jumps over\n\nPart 3: the lazy dog."
	if dispatched.Content != expectedContent {
		t.Fatalf("expected concatenated content:\n%q\ngot:\n%q", expectedContent, dispatched.Content)
	}
	if dispatched.MessageID != "103" {
		t.Fatalf("expected newest canonical message ID '103', got %q", dispatched.MessageID)
	}
}

// 3. Message after quiet window: becomes a separate logical turn.
func TestTelegramAggregator_MessageAfterQuietWindow(t *testing.T) {
	cap := &capturedCalls{}
	p := &mockPlatform{name: "telegram"}
	agg := NewTelegramAggregator(50*time.Millisecond, cap.Handler)

	// Send message 1
	agg.HandleMessage(p, &core.Message{
		SessionKey: "chat_123",
		Platform:   "telegram",
		UserID:     "user_1",
		MessageID:  "101",
		Content:    "First turn message",
	})

	// Wait beyond quiet window
	time.Sleep(80 * time.Millisecond)

	if cap.Count() != 1 {
		t.Fatalf("expected 1 dispatch for turn 1, got %d", cap.Count())
	}
	if cap.Get(0).Content != "First turn message" {
		t.Fatalf("turn 1 content mismatch: %q", cap.Get(0).Content)
	}

	// Send message 2 after quiet window
	agg.HandleMessage(p, &core.Message{
		SessionKey: "chat_123",
		Platform:   "telegram",
		UserID:     "user_1",
		MessageID:  "102",
		Content:    "Second turn message",
	})

	time.Sleep(80 * time.Millisecond)

	if cap.Count() != 2 {
		t.Fatalf("expected 2 separate dispatches, got %d", cap.Count())
	}
	if cap.Get(1).Content != "Second turn message" {
		t.Fatalf("turn 2 content mismatch: %q", cap.Get(1).Content)
	}
}

// 4. Different sessions: must never be merged together.
func TestTelegramAggregator_DifferentSessions(t *testing.T) {
	cap := &capturedCalls{}
	p := &mockPlatform{name: "telegram"}
	agg := NewTelegramAggregator(60*time.Millisecond, cap.Handler)

	agg.HandleMessage(p, &core.Message{
		SessionKey: "session_A",
		Platform:   "telegram",
		UserID:     "user_A",
		MessageID:  "A1",
		Content:    "Message from User A",
	})

	agg.HandleMessage(p, &core.Message{
		SessionKey: "session_B",
		Platform:   "telegram",
		UserID:     "user_B",
		MessageID:  "B1",
		Content:    "Message from User B",
	})

	time.Sleep(90 * time.Millisecond)

	if cap.Count() != 2 {
		t.Fatalf("expected 2 distinct dispatches for 2 sessions, got %d", cap.Count())
	}

	msg1 := cap.Get(0)
	msg2 := cap.Get(1)

	if msg1.SessionKey == msg2.SessionKey {
		t.Fatalf("sessions incorrectly merged: %s == %s", msg1.SessionKey, msg2.SessionKey)
	}
}

// 5. Slash commands / Attachments: immediately flush pending text and dispatch command.
func TestTelegramAggregator_SlashCommandBypass(t *testing.T) {
	cap := &capturedCalls{}
	p := &mockPlatform{name: "telegram"}
	agg := NewTelegramAggregator(100*time.Millisecond, cap.Handler)

	// Send conversational text
	agg.HandleMessage(p, &core.Message{
		SessionKey: "chat_123",
		Platform:   "telegram",
		UserID:     "user_1",
		MessageID:  "101",
		Content:    "Pending thought...",
	})

	// Before quiet window expires, send a slash command
	agg.HandleMessage(p, &core.Message{
		SessionKey: "chat_123",
		Platform:   "telegram",
		UserID:     "user_1",
		MessageID:  "102",
		Content:    "/reset",
	})

	// Both should be dispatched immediately:
	// Turn 1 = "Pending thought..."
	// Turn 2 = "/reset"
	if cap.Count() != 2 {
		t.Fatalf("expected immediate 2 dispatches on slash command, got %d", cap.Count())
	}

	if cap.Get(0).Content != "Pending thought..." {
		t.Fatalf("expected turn 1 to be flushed text, got %q", cap.Get(0).Content)
	}
	if cap.Get(1).Content != "/reset" {
		t.Fatalf("expected turn 2 to be slash command, got %q", cap.Get(1).Content)
	}
}

// 6. Stop / shutdown with buffered content: Flush() drains pending batches synchronously.
func TestTelegramAggregator_FlushOnShutdown(t *testing.T) {
	cap := &capturedCalls{}
	p := &mockPlatform{name: "telegram"}
	agg := NewTelegramAggregator(5*time.Second, cap.Handler) // Long quiet window

	agg.HandleMessage(p, &core.Message{
		SessionKey: "chat_123",
		Platform:   "telegram",
		UserID:     "user_1",
		MessageID:  "101",
		Content:    "Shutdown pending message",
	})

	// Flush immediately (simulating Platform.Stop())
	agg.Flush(p)

	if cap.Count() != 1 {
		t.Fatalf("expected 1 dispatch upon Flush(), got %d", cap.Count())
	}
	if cap.Get(0).Content != "Shutdown pending message" {
		t.Fatalf("flushed content mismatch: %q", cap.Get(0).Content)
	}
}
