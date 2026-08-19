package core

import (
	"context"
	"strings"
	"testing"
)

type failoverStubAgent struct {
	stubAgent
	providers   []ProviderConfig
	activeIdx   int
	switchCalls []string
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

type fakeCronAgentSession struct {
	stubAgentSession
	eventsCh chan Event
}

func (s *fakeCronAgentSession) Send(prompt string, messageID string, images []ImageAttachment, files []FileAttachment) error {
	go func() {
		s.eventsCh <- Event{Type: EventText, Content: "some response"}
		s.eventsCh <- Event{Type: EventResult, Done: true}
	}()
	return nil
}

func (s *fakeCronAgentSession) Events() <-chan Event {
	return s.eventsCh
}

func (s *fakeCronAgentSession) CurrentSessionID() string { return "fake-cron-session" }

type fakeCronAgent struct {
	failoverStubAgent
}

func (a *fakeCronAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return &fakeCronAgentSession{
		eventsCh: make(chan Event, 4),
	}, nil
}

type mockRecoveryDecider struct {
	decisions []CronRecoveryDecision
	calls     int
}

func (m *mockRecoveryDecider) DecideCronRecovery(_ context.Context, _ CronTurnOutcome) (CronRecoveryDecision, error) {
	if m.calls < len(m.decisions) {
		d := m.decisions[m.calls]
		m.calls++
		return d, nil
	}
	m.calls++
	return CronRecoveryDecision{}, nil
}

func TestParseProviderFailoverOptions(t *testing.T) {
	// []any form
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

func TestCronGenericRecoverySeam_RetryFlow(t *testing.T) {
	agent := &fakeCronAgent{
		failoverStubAgent: failoverStubAgent{
			providers: []ProviderConfig{
				{Name: "primary"},
				{Name: "fallback1"},
			},
			activeIdx: 0,
		},
	}
	eng := NewEngine("test", agent, []Platform{&stubCronReplyTargetPlatform{stubPlatformEngine: stubPlatformEngine{n: "stub"}}}, "", LangEnglish)
	eng.SetProviderFailover("primary", []string{"fallback1"})

	decider := &mockRecoveryDecider{
		decisions: []CronRecoveryDecision{
			{ShouldRetry: true, NextProvider: "fallback1"},
			{IsFailure: true, FailureReason: "retry also quota-walled"},
		},
	}
	eng.SetCronRecoveryDecider(decider)

	job := &CronJob{
		ID:          "test-cron-1",
		Project:     "test",
		SessionKey:  "stub:p:u",
		Prompt:      "test prompt",
		SessionMode: "new_per_run",
		Enabled:     true,
	}

	err := eng.ExecuteCronJob(job)
	if err == nil {
		t.Fatalf("expected error from retry failure, got nil")
	}
	if !strings.Contains(err.Error(), "retry also quota-walled") {
		t.Fatalf("expected retry failure reason in error, got: %v", err)
	}
	if decider.calls < 2 {
		t.Fatalf("expected at least 2 decider calls, got %d", decider.calls)
	}
	// Expected switches: reset to "primary" at start, then switch to "fallback1" for retry
	if len(agent.switchCalls) < 2 {
		t.Fatalf("expected at least 2 switch calls, got %v", agent.switchCalls)
	}
	if agent.switchCalls[0] != "primary" {
		t.Fatalf("first switch = %q, want primary reset %q", agent.switchCalls[0], "primary")
	}
	if agent.switchCalls[len(agent.switchCalls)-1] != "fallback1" {
		t.Fatalf("last switch = %q, want %q", agent.switchCalls[len(agent.switchCalls)-1], "fallback1")
	}
}
