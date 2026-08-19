package nexus

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type wrapperMockPlatform struct {
	mu          sync.Mutex
	name        string
	handler     core.MessageHandler
	stopped     bool
	dispatches  []string
	imagesSent  []string
	updatesSent []string
	buttonsSent [][][]core.ButtonOption
}

func (m *wrapperMockPlatform) Name() string { return m.name }
func (m *wrapperMockPlatform) Start(handler core.MessageHandler) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handler = handler
	return nil
}
func (m *wrapperMockPlatform) Reply(ctx context.Context, replyCtx any, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dispatches = append(m.dispatches, content)
	return nil
}
func (m *wrapperMockPlatform) Send(ctx context.Context, replyCtx any, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dispatches = append(m.dispatches, content)
	return nil
}
func (m *wrapperMockPlatform) SendWithButtons(ctx context.Context, replyCtx any, content string, buttons [][]core.ButtonOption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dispatches = append(m.dispatches, content)
	m.buttonsSent = append(m.buttonsSent, buttons)
	return nil
}
func (m *wrapperMockPlatform) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
	return nil
}
func (m *wrapperMockPlatform) ProgressStyle() string {
	return "compact"
}
func (m *wrapperMockPlatform) UpdateMessage(ctx context.Context, replyCtx any, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updatesSent = append(m.updatesSent, content)
	return nil
}
func (m *wrapperMockPlatform) SendImage(ctx context.Context, replyCtx any, img core.ImageAttachment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.imagesSent = append(m.imagesSent, img.FileName)
	return nil
}
func (m *wrapperMockPlatform) SendFile(ctx context.Context, replyCtx any, file core.FileAttachment) error {
	return nil
}
func (m *wrapperMockPlatform) SendAudio(ctx context.Context, replyCtx any, audio []byte, format string) error {
	return nil
}
func (m *wrapperMockPlatform) SendPreviewStart(ctx context.Context, replyCtx any, content string) (any, error) {
	return 1, nil
}
func (m *wrapperMockPlatform) DeletePreviewMessage(ctx context.Context, previewHandle any) error {
	return nil
}
func (m *wrapperMockPlatform) StartTyping(ctx context.Context, replyCtx any) (stop func()) {
	return func() {}
}
func (m *wrapperMockPlatform) RegisterCommands(commands []core.BotCommandInfo) error {
	return nil
}
func (m *wrapperMockPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return nil, nil
}

func waitForTurnCount(mu *sync.Mutex, turns *[]*core.Message, expected int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(*turns)
		mu.Unlock()
		if count >= expected {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// Simulate incoming message from underlying platform
func (m *wrapperMockPlatform) Emit(msg *core.Message) {
	m.mu.Lock()
	h := m.handler
	m.mu.Unlock()
	if h != nil {
		h(m, msg)
	}
}

// Test Wrapper E2E cases A-D
func TestDebouncedTelegramPlatform_E2EScenarios(t *testing.T) {
	// Case A: Single Message
	t.Run("CaseA_NormalSingleMessage", func(t *testing.T) {
		mock := &wrapperMockPlatform{name: "telegram"}
		wrapper := &DebouncedTelegramPlatform{
			underlying: mock,
			window:     50 * time.Millisecond,
		}

		var mu sync.Mutex
		var turns []*core.Message
		err := wrapper.Start(func(p core.Platform, msg *core.Message) {
			mu.Lock()
			defer mu.Unlock()
			turns = append(turns, msg)
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		mock.Emit(&core.Message{
			SessionKey: "chat_A",
			Platform:   "telegram",
			UserID:     "user_A",
			MessageID:  "1001",
			Content:    "Normal single message",
		})

		if !waitForTurnCount(&mu, &turns, 1, 500*time.Millisecond) {
			t.Fatalf("expected 1 turn within timeout")
		}

		mu.Lock()
		content := turns[0].Content
		mu.Unlock()

		if content != "Normal single message" {
			t.Fatalf("content mismatch: %q", content)
		}
	})

	// Case B: Rapid follow-up (3 messages in quick succession -> 1 combined turn)
	t.Run("CaseB_RapidFollowUp", func(t *testing.T) {
		mock := &wrapperMockPlatform{name: "telegram"}
		wrapper := &DebouncedTelegramPlatform{
			underlying: mock,
			window:     60 * time.Millisecond,
		}

		var mu sync.Mutex
		var turns []*core.Message
		_ = wrapper.Start(func(p core.Platform, msg *core.Message) {
			mu.Lock()
			defer mu.Unlock()
			turns = append(turns, msg)
		})

		mock.Emit(&core.Message{
			SessionKey: "chat_B",
			Platform:   "telegram",
			UserID:     "user_B",
			MessageID:  "2001",
			Content:    "Part 1",
		})
		time.Sleep(15 * time.Millisecond)

		mock.Emit(&core.Message{
			SessionKey: "chat_B",
			Platform:   "telegram",
			UserID:     "user_B",
			MessageID:  "2002",
			Content:    "Part 2",
		})
		time.Sleep(15 * time.Millisecond)

		mock.Emit(&core.Message{
			SessionKey: "chat_B",
			Platform:   "telegram",
			UserID:     "user_B",
			MessageID:  "2003",
			Content:    "Part 3",
		})

		if !waitForTurnCount(&mu, &turns, 1, 500*time.Millisecond) {
			t.Fatalf("expected 1 turn within timeout")
		}

		mu.Lock()
		count := len(turns)
		var msg *core.Message
		if count > 0 {
			msg = turns[0]
		}
		mu.Unlock()

		if count != 1 {
			t.Fatalf("expected 1 turn, got %d", count)
		}
		if msg.Content != "Part 1\n\nPart 2\n\nPart 3" {
			t.Fatalf("coalesced content mismatch: %q", msg.Content)
		}
		if msg.MessageID != "2003" {
			t.Fatalf("expected latest message ID 2003, got %q", msg.MessageID)
		}
	})

	// Case C: Delayed follow-up (2 turns separated by quiet window)
	t.Run("CaseC_DelayedFollowUp", func(t *testing.T) {
		mock := &wrapperMockPlatform{name: "telegram"}
		wrapper := &DebouncedTelegramPlatform{
			underlying: mock,
			window:     40 * time.Millisecond,
		}

		var mu sync.Mutex
		var turns []*core.Message
		_ = wrapper.Start(func(p core.Platform, msg *core.Message) {
			mu.Lock()
			defer mu.Unlock()
			turns = append(turns, msg)
		})

		mock.Emit(&core.Message{
			SessionKey: "chat_C",
			Platform:   "telegram",
			UserID:     "user_C",
			MessageID:  "3001",
			Content:    "Turn 1 text",
		})

		if !waitForTurnCount(&mu, &turns, 1, 300*time.Millisecond) {
			t.Fatalf("expected turn 1 within timeout")
		}

		mock.Emit(&core.Message{
			SessionKey: "chat_C",
			Platform:   "telegram",
			UserID:     "user_C",
			MessageID:  "3002",
			Content:    "Turn 2 text",
		})

		if !waitForTurnCount(&mu, &turns, 2, 300*time.Millisecond) {
			t.Fatalf("expected turn 2 within timeout")
		}
	})

	// Case D: Split chunk simulation (>4096 chars)
	t.Run("CaseD_LongTextSplit", func(t *testing.T) {
		mock := &wrapperMockPlatform{name: "telegram"}
		wrapper := &DebouncedTelegramPlatform{
			underlying: mock,
			window:     80 * time.Millisecond,
		}

		var mu sync.Mutex
		var turns []*core.Message
		_ = wrapper.Start(func(p core.Platform, msg *core.Message) {
			mu.Lock()
			defer mu.Unlock()
			turns = append(turns, msg)
		})

		chunk1 := strings.Repeat("X", 3500)
		chunk2 := strings.Repeat("Y", 3500)

		mock.Emit(&core.Message{
			SessionKey: "chat_D",
			Platform:   "telegram",
			UserID:     "user_D",
			MessageID:  "4001",
			Content:    chunk1,
		})
		time.Sleep(20 * time.Millisecond)

		mock.Emit(&core.Message{
			SessionKey: "chat_D",
			Platform:   "telegram",
			UserID:     "user_D",
			MessageID:  "4002",
			Content:    chunk2,
		})

		time.Sleep(120 * time.Millisecond)

		mu.Lock()
		count := len(turns)
		var mergedLen int
		if count > 0 {
			mergedLen = len(turns[0].Content)
		}
		mu.Unlock()

		if count != 1 {
			t.Fatalf("expected 1 turn, got %d", count)
		}
		if mergedLen != 7002 {
			t.Fatalf("expected merged length 7002, got %d", mergedLen)
		}
	})

	// Capability interface delegation test
	t.Run("CapabilityInterfaceDelegation", func(t *testing.T) {
		mock := &wrapperMockPlatform{name: "telegram"}
		var wrapper core.Platform = &DebouncedTelegramPlatform{
			underlying: mock,
			window:     50 * time.Millisecond,
		}

		if updater, ok := wrapper.(core.MessageUpdater); ok {
			_ = updater.UpdateMessage(context.Background(), nil, "updated content")
		} else {
			t.Fatalf("wrapper failed MessageUpdater assertion")
		}

		if sender, ok := wrapper.(core.ImageSender); ok {
			_ = sender.SendImage(context.Background(), nil, core.ImageAttachment{FileName: "test.png"})
		} else {
			t.Fatalf("wrapper failed ImageSender assertion")
		}

		if provider, ok := wrapper.(core.ProgressStyleProvider); ok {
			if style := provider.ProgressStyle(); style != "compact" {
				t.Fatalf("unexpected progress style: %q", style)
			}
		} else {
			t.Fatalf("wrapper failed ProgressStyleProvider assertion")
		}

		if quietProv, ok := wrapper.(core.QuietSeparatorProvider); ok {
			if sep := quietProv.QuietSeparator(); sep != "\n" {
				t.Fatalf("unexpected quiet separator: %q, want %q", sep, "\n")
			}
		} else {
			t.Fatalf("wrapper failed QuietSeparatorProvider assertion")
		}

		mock.mu.Lock()
		updateCount := len(mock.updatesSent)
		imageCount := len(mock.imagesSent)
		mock.mu.Unlock()

		if updateCount != 1 || imageCount != 1 {
			t.Fatalf("capability delegation failed: updates=%d images=%d", updateCount, imageCount)
		}
	})
}
