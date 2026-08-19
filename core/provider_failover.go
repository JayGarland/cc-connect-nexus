package core

import (
	"context"
)

// CronTurnOutcome holds the execution outcome of a cron turn for recovery evaluation.
type CronTurnOutcome struct {
	Job            *CronJob
	Session        *Session
	Agent          Agent
	AgentSessionID string
	EmptyResponse  bool
}

// CronRecoveryDecision describes the action to take after a cron turn completes.
type CronRecoveryDecision struct {
	ShouldRetry   bool
	NextProvider  string
	IsFailure     bool
	FailureReason string
}

// CronRecoveryDecider inspects a cron turn's outcome and decides whether to retry or fail.
type CronRecoveryDecider interface {
	DecideCronRecovery(ctx context.Context, outcome CronTurnOutcome) (CronRecoveryDecision, error)
}

var defaultCronRecoveryDeciderFactory func(primary string, fallbacks []string) CronRecoveryDecider

// RegisterDefaultCronRecoveryDecider registers a global factory function for creating default
// CronRecoveryDecider instances when SetProviderFailover is called on an Engine.
func RegisterDefaultCronRecoveryDecider(factory func(primary string, fallbacks []string) CronRecoveryDecider) {
	defaultCronRecoveryDeciderFactory = factory
}

// ParseProviderFailoverOptions parses provider and fallback_providers from an agent options map.
func ParseProviderFailoverOptions(opts map[string]any) (primary string, fallback []string) {
	if opts == nil {
		return "", nil
	}
	if v, ok := opts["provider"].(string); ok {
		primary = v
	}
	switch v := opts["fallback_providers"].(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				fallback = append(fallback, s)
			}
		}
	case []string:
		fallback = append(fallback, v...)
	case string:
		if v != "" {
			fallback = append(fallback, v)
		}
	}
	return primary, fallback
}
