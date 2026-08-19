package recovery

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// Session limit patterns matching Claude Code quota wall termination.
var sessionLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)you'?ve hit your session limit`),
	regexp.MustCompile(`(?i)you have hit your session limit`),
	regexp.MustCompile(`(?i)session limit reached`),
}

// NexusCronRecoveryDecider implements core.CronRecoveryDecider for Claude Code.
// It inspects session history for Claude Code quota wall patterns and decides
// whether to fail over to a fallback provider in a fresh side session.
type NexusCronRecoveryDecider struct {
	primary   string
	fallbacks []string
}

// NewNexusCronRecoveryDecider creates a new recovery decider instance.
func NewNexusCronRecoveryDecider(primary string, fallbacks []string) core.CronRecoveryDecider {
	return &NexusCronRecoveryDecider{
		primary:   primary,
		fallbacks: fallbacks,
	}
}

// DecideCronRecovery inspects the turn outcome. If a session limit quota wall is confirmed,
// it selects the next fallback provider for a single retry or declares a failure if exhausted.
func (d *NexusCronRecoveryDecider) DecideCronRecovery(ctx context.Context, outcome core.CronTurnOutcome) (core.CronRecoveryDecision, error) {
	if outcome.Agent == nil {
		if outcome.EmptyResponse {
			return core.CronRecoveryDecision{IsFailure: true, FailureReason: "empty response"}, nil
		}
		return core.CronRecoveryDecision{}, nil
	}

	isLimitWall := false

	// Attempt 1: check HistoryProvider (e.g. Claude Code agent reading from transcript JSONL)
	if historyProv, ok := outcome.Agent.(core.HistoryProvider); ok && outcome.AgentSessionID != "" {
		entries, err := historyProv.GetSessionHistory(ctx, outcome.AgentSessionID, 1)
		if err != nil {
			slog.Warn("nexus recovery: error reading session history", "error", err, "session_id", outcome.AgentSessionID)
		} else {
			for i := len(entries) - 1; i >= 0; i-- {
				if entries[i].Role != "assistant" {
					continue
				}
				content := strings.TrimSpace(entries[i].Content)
				if content == "" {
					continue
				}
				if len(content) <= 200 {
					for _, re := range sessionLimitPatterns {
						if re.MatchString(content) {
							isLimitWall = true
							break
						}
					}
				}
				break
			}
		}
	}

	// Attempt 2: check outcome.Session.History if HistoryProvider did not match
	if !isLimitWall && outcome.Session != nil {
		history := outcome.Session.History
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Role != "assistant" {
				continue
			}
			content := strings.TrimSpace(history[i].Content)
			if content == "" {
				continue
			}
			if len(content) <= 200 {
				for _, re := range sessionLimitPatterns {
					if re.MatchString(content) {
						isLimitWall = true
						break
					}
				}
			}
			break
		}
	}

	if isLimitWall {
		// Quota wall confirmed!
		if len(d.fallbacks) == 0 {
			return core.CronRecoveryDecision{
				IsFailure:     true,
				FailureReason: "session quota limit reached (no fallback provider configured)",
			}, nil
		}

		current := ""
		if switcher, ok := outcome.Agent.(core.ProviderSwitcher); ok {
			if cur := switcher.GetActiveProvider(); cur != nil {
				current = cur.Name
			}
		}

		next := ""
		for _, name := range d.fallbacks {
			if name != current {
				next = name
				break
			}
		}

		if next == "" {
			return core.CronRecoveryDecision{
				IsFailure:     true,
				FailureReason: "session quota limit reached (fallback providers exhausted or invalid)",
			}, nil
		}

		return core.CronRecoveryDecision{
			ShouldRetry:  true,
			NextProvider: next,
		}, nil
	}

	if outcome.EmptyResponse {
		return core.CronRecoveryDecision{
			IsFailure:     true,
			FailureReason: "empty response",
		}, nil
	}

	return core.CronRecoveryDecision{}, nil
}
