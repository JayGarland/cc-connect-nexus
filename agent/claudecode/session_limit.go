package claudecode

import (
	"context"
	"regexp"
	"strings"
)

// sessionLimitPatterns match the Claude Code session-quota and rate-limit walls.
var sessionLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)you'?ve hit your session limit`),
	regexp.MustCompile(`(?i)you have hit your session limit`),
	regexp.MustCompile(`(?i)session limit reached`),
	regexp.MustCompile(`(?i)usage limit reached`),
	regexp.MustCompile(`(?i)rate limit exceeded`),
	regexp.MustCompile(`(?i)quota exhausted`),
	regexp.MustCompile(`(?i)resets \d`),
	regexp.MustCompile(`(?i)resets in \d`),
	regexp.MustCompile(`(?i)resets at \d`),
}

// IsSessionLimitEnding reports whether the given session's response is a
// session-quota wall termination rather than a real reply.
//
// Heuristics:
//   - Fast path: inspects the in-memory response text directly (0 disk I/O).
//   - Fallback path: inspects the session transcript on disk if in-memory text is empty.
//   - The content must match a quota/limit pattern AND be short (< 300 chars)
//     without code fences (```).
func (a *Agent) IsSessionLimitEnding(ctx context.Context, sessionID, content string) (bool, error) {
	if text := strings.TrimSpace(content); text != "" {
		if isQuotaWallText(text) {
			return true, nil
		}
	}

	if sessionID == "" {
		return false, nil
	}
	entries, err := a.GetSessionHistory(ctx, sessionID, 1)
	if err != nil {
		return false, err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Role != "assistant" {
			continue
		}
		if isQuotaWallText(entries[i].Content) {
			return true, nil
		}
		return false, nil
	}
	return false, nil
}

func isQuotaWallText(content string) bool {
	text := strings.TrimSpace(content)
	if text == "" || len(text) > 300 || strings.Contains(text, "```") {
		return false
	}
	for _, re := range sessionLimitPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}
