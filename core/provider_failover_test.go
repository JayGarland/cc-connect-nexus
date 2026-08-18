package core

import (
	"context"
	"testing"
)

// failoverStubAgent is a stub Agent implementing ProviderSwitcher +
// SessionLimitDetector to exercise the cron failover path without real
// processes.
type failoverStubAgent struct {
	stubAgent
	providers    []ProviderConfig
	activeIdx    int
	limitHit     bool
	limitChecked int
	switchCalls  []string
}

func (a *failoverStubAgent) SetProviders(providers []ProviderConfig) {
	a.providers = providers
}

func (a *failoverStubAgent) SetActiveProvider(name string) bool {
	for i, p := range a.providers {
		if p.Name == name {
			a.activeIdx = i
			a.switchCalls = append(a.switchCalls, name)
			return true
		}
	}
	return false
}

func (a *failoverStubAgent) GetActiveProvider() *ProviderConfig {
	if a.activeIdx < 0 || a.activeIdx >= len(a.providers) {
		return nil
	}
	p := a.providers[a.activeIdx]
	return &p
}

func (a *failoverStubAgent) ListProviders() []ProviderConfig { return a.providers }

func (a *failoverStubAgent) IsSessionLimitEnding(_ context.Context, _ string) (bool, error) {
	a.limitChecked++
	return a.limitHit, nil
}

// fakeCronAgentSession yields an already-closed Events channel so a turn
// completes with no assistant reply (the empty-response condition that
// triggers the limit check).
type fakeCronAgentSession struct {
	stubAgentSession
}

func (s *fakeCronAgentSession) Events() <-chan Event {
	ch := make(chan Event)
	close(ch)
	return ch
}

func (s *fakeCronAgentSession) CurrentSessionID() string { return "fake-cron-session" }

type fakeCronAgent struct {
	failoverStubAgent
}

func (a *fakeCronAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return &fakeCronAgentSession{}, nil
}

// wireFailover is the test-side twin of the cmd wiring: parse options and
// apply them to the engine.
func wireFailover(eng *Engine, opts map[string]any) {
	primary, fallback := ParseProviderFailoverOptions(opts)
	eng.SetProviderFailover(primary, fallback)
}

// TestCronProviderFailover exercises ExecuteCronJob's new-per-run path:
// when the turn ends empty AND the agent reports a session-limit wall AND a
// fallback provider is configured, the engine must switch provider and
// retry the job once.
func TestCronProviderFailover(t *testing.T) {
	agent := &fakeCronAgent{
		failoverStubAgent: failoverStubAgent{
			providers: []ProviderConfig{
				{Name: "primary"},
				{Name: "fallback1"},
			},
			activeIdx: 0,
			limitHit:  true,
		},
	}
	eng := NewEngine("test", agent, []Platform{&stubCronReplyTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "stub"}}}, "", LangEnglish)
	eng.SetProviderFailover("primary", []string{"fallback1"})

	job := &CronJob{
		ID:         "test-cron-1",
		Project:    "test",
		SessionKey: "stub:p:u",
		Prompt:     "Standing Participation v1 scheduled check.",
		Enabled:    true,
	}
	job.SessionMode = "new_per_run"

	err := eng.ExecuteCronJob(job)
	if err == nil {
		t.Fatalf("expected an error (both attempts end empty), got nil")
	}
	if agent.limitChecked == 0 {
		t.Fatalf("session-limit detector was never consulted")
	}
	// Expected switches: the primary reset before the run, then the failover
	// switch to fallback1 for the retry.
	if len(agent.switchCalls) < 2 {
		t.Fatalf("expected primary reset + failover switch, got %v", agent.switchCalls)
	}
	if agent.switchCalls[0] != "primary" {
		t.Fatalf("first switch = %q, want primary reset %q", agent.switchCalls[0], "primary")
	}
	if agent.switchCalls[len(agent.switchCalls)-1] != "fallback1" {
		t.Fatalf("last switch = %q, want %q", agent.switchCalls[len(agent.switchCalls)-1], "fallback1")
	}
}

// TestCronNoFailoverWithoutConfig verifies that without configured fallback
// providers the engine does NOT switch providers on a limit wall — it just
// reports the empty-response error as today.
func TestCronNoFailoverWithoutConfig(t *testing.T) {
	agent := &fakeCronAgent{
		failoverStubAgent: failoverStubAgent{
			providers: []ProviderConfig{
				{Name: "primary"},
				{Name: "fallback1"},
			},
			activeIdx: 0,
			limitHit:  true,
		},
	}
	eng := NewEngine("test", agent, []Platform{&stubCronReplyTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "stub"}}}, "", LangEnglish)

	job := &CronJob{
		ID:         "test-cron-2",
		Project:    "test",
		SessionKey: "stub:p:u",
		Prompt:     "check.",
		Enabled:    true,
	}
	job.SessionMode = "new_per_run"

	err := eng.ExecuteCronJob(job)
	if err == nil {
		t.Fatalf("expected an empty-response error, got nil")
	}
	// No primary is configured in this test, and no failover should occur —
	// so no switches at all.
	if len(agent.switchCalls) != 0 {
		t.Fatalf("provider switched without fallback config: %v", agent.switchCalls)
	}
}

// TestCronNoFailoverWhenNotLimitWall verifies that an empty response which is
// NOT a session-limit wall does not trigger a provider switch even when a
// fallback chain is configured.
func TestCronNoFailoverWhenNotLimitWall(t *testing.T) {
	agent := &fakeCronAgent{
		failoverStubAgent: failoverStubAgent{
			providers: []ProviderConfig{
				{Name: "primary"},
				{Name: "fallback1"},
			},
			activeIdx: 0,
			limitHit:  false,
		},
	}
	eng := NewEngine("test", agent, []Platform{&stubCronReplyTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "stub"}}}, "", LangEnglish)
	eng.SetProviderFailover("primary", []string{"fallback1"})

	job := &CronJob{
		ID:         "test-cron-3",
		Project:    "test",
		SessionKey: "stub:p:u",
		Prompt:     "check.",
		Enabled:    true,
	}
	job.SessionMode = "new_per_run"

	err := eng.ExecuteCronJob(job)
	if err == nil {
		t.Fatalf("expected an empty-response error, got nil")
	}
	// Primary reset before the run is expected; a failover switch is not.
	for _, s := range agent.switchCalls {
		if s != "primary" {
			t.Fatalf("provider switched to %q on a non-limit empty response: %v", s, agent.switchCalls)
		}
	}
}

// TestParseProviderFailoverOptions covers the option-parsing helper.
func TestParseProviderFailoverOptions(t *testing.T) {
	// []any form (as TOML arrays parse in this codebase's loader).
	primary, fallback := ParseProviderFailoverOptions(map[string]any{
		"provider":           "p",
		"fallback_providers": []any{"f1", "f2"},
	})
	if primary != "p" {
		t.Fatalf("primary = %q, want %q", primary, "p")
	}
	if len(fallback) != 2 || fallback[0] != "f1" || fallback[1] != "f2" {
		t.Fatalf("fallback = %v, want [f1 f2]", fallback)
	}

	// string form
	_, fallback = ParseProviderFailoverOptions(map[string]any{"fallback_providers": "f1"})
	if len(fallback) != 1 || fallback[0] != "f1" {
		t.Fatalf("string-form fallback = %v, want [f1]", fallback)
	}

	// absent => disabled
	primary, fallback = ParseProviderFailoverOptions(map[string]any{})
	if primary != "" || len(fallback) != 0 {
		t.Fatalf("absent options must leave failover disabled")
	}
}

