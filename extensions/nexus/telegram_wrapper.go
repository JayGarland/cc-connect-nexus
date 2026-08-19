package nexus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/platform/telegram"
)

// DebouncedTelegramPlatform wraps an underlying Telegram platform to aggregate
// rapid consecutive text messages during a quiet window.
type DebouncedTelegramPlatform struct {
	underlying core.Platform
	aggregator *TelegramAggregator
	window     time.Duration
}

// NewTelegramWrapper is the platform factory registered with core.RegisterPlatform.
// It parses aggregation policies from config.toml platform options.
func NewTelegramWrapper(opts map[string]any) (core.Platform, error) {
	underlying, err := telegram.New(opts)
	if err != nil {
		return nil, err
	}

	var window time.Duration
	aggEnabled := true
	if rawAgg, ok := opts["nexus_aggregation_enabled"]; ok {
		if b, ok := coerceBool(rawAgg); ok {
			aggEnabled = b
		}
	}
	if aggEnabled {
		if raw, ok := opts["text_batch_window_ms"]; ok {
			if ms, err := coerceMilliseconds(raw); err != nil {
				return nil, fmt.Errorf("nexus: invalid text_batch_window_ms %v: %w", raw, err)
			} else if ms > 0 {
				window = time.Duration(ms) * time.Millisecond
			}
		}
	}

	return &DebouncedTelegramPlatform{
		underlying: underlying,
		window:     window,
	}, nil
}

func (p *DebouncedTelegramPlatform) Name() string {
	return p.underlying.Name()
}

func (p *DebouncedTelegramPlatform) Window() time.Duration {
	return p.window
}

func (p *DebouncedTelegramPlatform) Start(handler core.MessageHandler) error {
	if p.window > 0 {
		p.aggregator = NewTelegramAggregator(p.window, handler)
		return p.underlying.Start(p.aggregator.HandleMessage)
	}
	return p.underlying.Start(handler)
}

func (p *DebouncedTelegramPlatform) Reply(ctx context.Context, replyCtx any, content string) error {
	return p.underlying.Reply(ctx, replyCtx, content)
}

func (p *DebouncedTelegramPlatform) Send(ctx context.Context, replyCtx any, content string) error {
	return p.underlying.Send(ctx, replyCtx, content)
}

func (p *DebouncedTelegramPlatform) Stop() error {
	if p.aggregator != nil {
		p.aggregator.Flush(p.underlying)
	}
	return p.underlying.Stop()
}

// Forward optional capability interfaces supported by telegram.Platform

func (p *DebouncedTelegramPlatform) SetLifecycleHandler(h core.PlatformLifecycleHandler) {
	if a, ok := p.underlying.(core.AsyncRecoverablePlatform); ok {
		a.SetLifecycleHandler(h)
	}
}

func (p *DebouncedTelegramPlatform) UpdateMessage(ctx context.Context, replyCtx any, content string) error {
	if u, ok := p.underlying.(core.MessageUpdater); ok {
		return u.UpdateMessage(ctx, replyCtx, content)
	}
	return core.ErrNotSupported
}

func (p *DebouncedTelegramPlatform) SendWithButtons(ctx context.Context, replyCtx any, content string, buttons [][]core.ButtonOption) error {
	if s, ok := p.underlying.(core.InlineButtonSender); ok {
		return s.SendWithButtons(ctx, replyCtx, content, buttons)
	}
	return core.ErrNotSupported
}

func (p *DebouncedTelegramPlatform) SendImage(ctx context.Context, replyCtx any, img core.ImageAttachment) error {
	if s, ok := p.underlying.(core.ImageSender); ok {
		return s.SendImage(ctx, replyCtx, img)
	}
	return core.ErrNotSupported
}

func (p *DebouncedTelegramPlatform) SendFile(ctx context.Context, replyCtx any, file core.FileAttachment) error {
	if s, ok := p.underlying.(core.FileSender); ok {
		return s.SendFile(ctx, replyCtx, file)
	}
	return core.ErrNotSupported
}

func (p *DebouncedTelegramPlatform) SendAudio(ctx context.Context, replyCtx any, audio []byte, format string) error {
	if s, ok := p.underlying.(core.AudioSender); ok {
		return s.SendAudio(ctx, replyCtx, audio, format)
	}
	return core.ErrNotSupported
}

func (p *DebouncedTelegramPlatform) StartTyping(ctx context.Context, replyCtx any) (stop func()) {
	if t, ok := p.underlying.(core.TypingIndicator); ok {
		return t.StartTyping(ctx, replyCtx)
	}
	return func() {}
}

func (p *DebouncedTelegramPlatform) ProgressStyle() string {
	if pr, ok := p.underlying.(core.ProgressStyleProvider); ok {
		return pr.ProgressStyle()
	}
	return "compact"
}

func (p *DebouncedTelegramPlatform) RegisterCommands(commands []core.BotCommandInfo) error {
	if r, ok := p.underlying.(core.CommandRegistrar); ok {
		return r.RegisterCommands(commands)
	}
	return core.ErrNotSupported
}

func (p *DebouncedTelegramPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	if r, ok := p.underlying.(core.ReplyContextReconstructor); ok {
		return r.ReconstructReplyCtx(sessionKey)
	}
	return nil, core.ErrNotSupported
}

func (p *DebouncedTelegramPlatform) SendPreviewStart(ctx context.Context, replyCtx any, content string) (any, error) {
	if s, ok := p.underlying.(core.PreviewStarter); ok {
		return s.SendPreviewStart(ctx, replyCtx, content)
	}
	return nil, core.ErrNotSupported
}

func (p *DebouncedTelegramPlatform) DeletePreviewMessage(ctx context.Context, previewHandle any) error {
	if c, ok := p.underlying.(core.PreviewCleaner); ok {
		return c.DeletePreviewMessage(ctx, previewHandle)
	}
	return core.ErrNotSupported
}

func coerceBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		}
	case int:
		return x != 0, true
	case int32:
		return x != 0, true
	case int64:
		return x != 0, true
	case uint:
		return x != 0, true
	case uint32:
		return x != 0, true
	case uint64:
		return x != 0, true
	case float32:
		return x != 0, true
	case float64:
		return x != 0, true
	}
	return false, false
}

func coerceMilliseconds(v any) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		return int64(x), nil
	case float32:
		return int64(x), nil
	case float64:
		return int64(x), nil
	default:
		return 0, fmt.Errorf("expected number, got %T", v)
	}
}

// Compile-time interface assertions
var (
	_ core.Platform                  = (*DebouncedTelegramPlatform)(nil)
	_ core.AsyncRecoverablePlatform  = (*DebouncedTelegramPlatform)(nil)
	_ core.MessageUpdater            = (*DebouncedTelegramPlatform)(nil)
	_ core.InlineButtonSender        = (*DebouncedTelegramPlatform)(nil)
	_ core.PreviewStarter            = (*DebouncedTelegramPlatform)(nil)
	_ core.PreviewCleaner            = (*DebouncedTelegramPlatform)(nil)
	_ core.ImageSender               = (*DebouncedTelegramPlatform)(nil)
	_ core.FileSender                = (*DebouncedTelegramPlatform)(nil)
	_ core.AudioSender               = (*DebouncedTelegramPlatform)(nil)
	_ core.TypingIndicator           = (*DebouncedTelegramPlatform)(nil)
	_ core.ProgressStyleProvider     = (*DebouncedTelegramPlatform)(nil)
	_ core.CommandRegistrar          = (*DebouncedTelegramPlatform)(nil)
	_ core.ReplyContextReconstructor = (*DebouncedTelegramPlatform)(nil)
)
