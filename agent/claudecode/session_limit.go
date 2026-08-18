package claudecode

import (
	"context"
	"regexp"
	"strings"
)

// sessionLimitPatterns match the Claude Code session-quota wall. The
// authoritative signal is the exact message Claude Code emits when the
// subscription session budget is exhausted; the secondary patterns cover
// localized/paraphrased variants without being so broad that a real reply
// mentioning the phrase matches.
var sessionLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)you'?ve hit your session limit`),
	regexp.MustCompile(`(?i)you have hit your session limit`),
	regexp.MustCompile(`(?i)session limit reached`),
}

// IsSessionLimitEnding reports whether the given session's last assistant
// message is a session-quota wall termination rather than a real reply.
//
// Heuristics (deliberately conservative):
//   - Only the last assistant entry is examined.
//   - The content must match a session-limit pattern AND be short
//     (< 200 chars). A real reply that merely quotes the phrase as part of
//     a larger answer is long and structured, so it is not treated as a
//     quota wall.
//   - A missing/unreadable transcript returns (false, error) so callers
//     can distinguish "not a limit ending" from "cannot tell".
func (a *Agent) IsSessionLimitEnding(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	entries, err := a.GetSessionHistory(ctx, sessionID, 1)
	if err != nil {
		return false, err
	}
	// Scan backwards for the last assistant entry (limit=1 already returns
	// the tail, but role-filter defensively).
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Role != "assistant" {
			continue
		}
		content := strings.TrimSpace(entries[i].Content)
		if content == "" {
			continue
		}
		if len(content) > 200 {
			return false, nil
		}
		for _, re := range sessionLimitPatterns {
			if re.MatchString(content) {
				return true, nil
			}
		}
		return false, nil
	}
	return false, nil
}
