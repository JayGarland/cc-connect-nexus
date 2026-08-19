package core

import (
	"testing"
)

type mockQuietPlatform struct {
	stubPlatformEngine
	sep string
}

func (m *mockQuietPlatform) QuietSeparator() string {
	return m.sep
}

var _ QuietSeparatorProvider = (*mockQuietPlatform)(nil)

func TestQuietSeparator_Resolution(t *testing.T) {
	// 1. Default behavior unchanged: platforms without QuietSeparatorProvider get "\n\n"
	defaultPlat := &stubPlatformEngine{n: "slack"}
	if got := quietSeparator(defaultPlat); got != "\n\n" {
		t.Fatalf("expected default separator %q, got %q", "\n\n", got)
	}

	// 2. Custom separator configured (e.g. "\n" for compact Telegram layout)
	customPlat := &mockQuietPlatform{stubPlatformEngine: stubPlatformEngine{n: "telegram"}, sep: "\n"}
	if got := quietSeparator(customPlat); got != "\n" {
		t.Fatalf("expected custom separator %q, got %q", "\n", got)
	}

	// 3. Empty custom separator falls back to default "\n\n"
	emptyPlat := &mockQuietPlatform{stubPlatformEngine: stubPlatformEngine{n: "telegram"}, sep: ""}
	if got := quietSeparator(emptyPlat); got != "\n\n" {
		t.Fatalf("expected fallback separator %q, got %q", "\n\n", got)
	}
}

func TestQuietSeparator_StreamPreviewAppend(t *testing.T) {
	sp := &streamPreview{
		cfg: StreamPreviewCfg{
			Enabled: true,
		},
		fullText: "First paragraph.",
	}

	// Append separator
	added := sp.appendSeparator("\n")
	if !added {
		t.Fatalf("expected appendSeparator to return true")
	}
	if sp.fullText != "First paragraph.\n" {
		t.Fatalf("fullText = %q, want %q", sp.fullText, "First paragraph.\n")
	}

	// Verify append when fullText is empty (does not prepend separator to empty string)
	emptySp := &streamPreview{
		cfg: StreamPreviewCfg{
			Enabled: true,
		},
		fullText: "",
	}
	if emptySp.appendSeparator("\n") {
		t.Fatalf("expected appendSeparator on empty fullText to return false")
	}
}
