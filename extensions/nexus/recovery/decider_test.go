package recovery

import (
	"context"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

type mockHistoryAgent struct {
	history  []core.HistoryEntry
	provider *core.ProviderConfig
}

func (m *mockHistoryAgent) Name() string { return "claudecode" }
func (m *mockHistoryAgent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	return nil, nil
}
func (m *mockHistoryAgent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}
func (m *mockHistoryAgent) Stop() error { return nil }
func (m *mockHistoryAgent) GetSessionHistory(ctx context.Context, sessionID string, limit int) ([]core.HistoryEntry, error) {
	return m.history, nil
}
func (m *mockHistoryAgent) SetProviders(providers []core.ProviderConfig) {}
func (m *mockHistoryAgent) SetActiveProvider(name string) bool           { return true }
func (m *mockHistoryAgent) GetActiveProvider() *core.ProviderConfig      { return m.provider }
func (m *mockHistoryAgent) ListProviders() []core.ProviderConfig         { return nil }

func TestNexusCronRecoveryDecider_QuotaWallClassification(t *testing.T) {
	tests := []struct {
		name          string
		assistantText string
		fallbacks     []string
		currentProv   string
		empty         bool
		wantRetry     bool
		wantNext      string
		wantFailure   bool
	}{
		{
			name:          "Normal turn -> no retry, no failure",
			assistantText: "Here is the summary of git status: clean.",
			fallbacks:     []string{"fallback1"},
			currentProv:   "primary",
			empty:         false,
			wantRetry:     false,
			wantNext:      "",
			wantFailure:   false,
		},
		{
			name:          "Quota wall with fallback -> retry on next provider",
			assistantText: "You've hit your session limit · resets 10am (Europe/Paris)",
			fallbacks:     []string{"fallback1", "fallback2"},
			currentProv:   "primary",
			empty:         false,
			wantRetry:     true,
			wantNext:      "fallback1",
			wantFailure:   false,
		},
		{
			name:          "Quota wall without fallback -> hard failure",
			assistantText: "You've hit your session limit · resets 10am (Europe/Paris)",
			fallbacks:     nil,
			currentProv:   "primary",
			empty:         false,
			wantRetry:     false,
			wantNext:      "",
			wantFailure:   true,
		},
		{
			name:          "Quota wall with exhausted fallback -> hard failure",
			assistantText: "Session limit reached",
			fallbacks:     []string{"primary"}, // only current provider configured
			currentProv:   "primary",
			empty:         false,
			wantRetry:     false,
			wantNext:      "",
			wantFailure:   true,
		},
		{
			name:          "Normal empty response -> failure (empty response)",
			assistantText: "",
			fallbacks:     []string{"fallback1"},
			currentProv:   "primary",
			empty:         true,
			wantRetry:     false,
			wantNext:      "",
			wantFailure:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &mockHistoryAgent{
				provider: &core.ProviderConfig{Name: tt.currentProv},
			}
			if tt.assistantText != "" {
				agent.history = []core.HistoryEntry{
					{Role: "user", Content: "check"},
					{Role: "assistant", Content: tt.assistantText},
				}
			}

			decider := NewNexusCronRecoveryDecider("primary", tt.fallbacks)
			outcome := core.CronTurnOutcome{
				Agent:          agent,
				AgentSessionID: "test-agent-session",
				EmptyResponse:  tt.empty,
			}

			decision, err := decider.DecideCronRecovery(context.Background(), outcome)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if decision.ShouldRetry != tt.wantRetry {
				t.Errorf("ShouldRetry = %v, want %v", decision.ShouldRetry, tt.wantRetry)
			}
			if decision.NextProvider != tt.wantNext {
				t.Errorf("NextProvider = %q, want %q", decision.NextProvider, tt.wantNext)
			}
			if decision.IsFailure != tt.wantFailure {
				t.Errorf("IsFailure = %v, want %v", decision.IsFailure, tt.wantFailure)
			}
		})
	}
}
