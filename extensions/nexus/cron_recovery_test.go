package nexus

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

type fullMockHistoryAgent struct {
	mu          sync.Mutex
	providers   []core.ProviderConfig
	activeIdx   int
	switchCalls []string
	history     map[string][]core.HistoryEntry
	turnOutputs []string // outputs emitted across turns
	turnIndex   int
	startCalls  int
}

func (a *fullMockHistoryAgent) Name() string { return "claudecode" }
func (a *fullMockHistoryAgent) Stop() error  { return nil }
func (a *fullMockHistoryAgent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}
func (a *fullMockHistoryAgent) SetProviders(providers []core.ProviderConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.providers = providers
}
func (a *fullMockHistoryAgent) SetActiveProvider(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, p := range a.providers {
		if p.Name == name {
			a.activeIdx = i
			a.switchCalls = append(a.switchCalls, name)
			return true
		}
	}
	return false
}
func (a *fullMockHistoryAgent) GetActiveProvider() *core.ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeIdx < 0 || a.activeIdx >= len(a.providers) {
		return nil
	}
	p := a.providers[a.activeIdx]
	return &p
}
func (a *fullMockHistoryAgent) ListProviders() []core.ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.providers
}
func (a *fullMockHistoryAgent) GetSessionHistory(ctx context.Context, sessionID string, limit int) ([]core.HistoryEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.history != nil {
		return a.history[sessionID], nil
	}
	return nil, nil
}

type fullMockSession struct {
	agent     *fullMockHistoryAgent
	sessionID string
	eventsCh  chan core.Event
}

func (s *fullMockSession) Send(prompt string, messageID string, images []core.ImageAttachment, files []core.FileAttachment) error {
	s.agent.mu.Lock()
	defer s.agent.mu.Unlock()

	var outputText string
	if s.agent.turnIndex < len(s.agent.turnOutputs) {
		outputText = s.agent.turnOutputs[s.agent.turnIndex]
		s.agent.turnIndex++
	}

	if s.agent.history == nil {
		s.agent.history = make(map[string][]core.HistoryEntry)
	}

	s.agent.history[s.sessionID] = []core.HistoryEntry{
		{Role: "user", Content: prompt},
		{Role: "assistant", Content: outputText},
	}

	go func(text string) {
		if text != "" {
			s.eventsCh <- core.Event{Type: core.EventText, Content: text}
		}
		s.eventsCh <- core.Event{Type: core.EventResult, Done: true}
	}(outputText)

	return nil
}

func (s *fullMockSession) RespondPermission(requestID string, result core.PermissionResult) error {
	return nil
}
func (s *fullMockSession) Events() <-chan core.Event { return s.eventsCh }
func (s *fullMockSession) CurrentSessionID() string  { return s.sessionID }
func (s *fullMockSession) Alive() bool               { return true }
func (s *fullMockSession) Close() error              { return nil }

func (a *fullMockHistoryAgent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	a.startCalls++
	sid := sessionID
	if sid == "" {
		sid = "mock-sid"
	}
	a.mu.Unlock()

	return &fullMockSession{
		agent:     a,
		sessionID: sid,
		eventsCh:  make(chan core.Event, 8),
	}, nil
}

type cronMockPlatform struct {
	mockPlatform
}

func (m *cronMockPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return "mock-reply-ctx", nil
}

var _ core.ReplyContextReconstructor = (*cronMockPlatform)(nil)

func createTestEngine(agent core.Agent, primary string, fallbacks []string) *core.Engine {
	plat := &cronMockPlatform{mockPlatform: mockPlatform{name: "telegram"}}
	eng := core.NewEngine("test-proj", agent, []core.Platform{plat}, "", core.LangEnglish)
	eng.SetProviderFailover(primary, fallbacks)
	return eng
}

// 1. Normal Claude cron turn
func TestCronScenarios_NormalTurn(t *testing.T) {
	agent := &fullMockHistoryAgent{
		providers: []core.ProviderConfig{
			{Name: "primary"},
			{Name: "fallback1"},
		},
		activeIdx:   0,
		turnOutputs: []string{"Daily report: all green."},
	}
	eng := createTestEngine(agent, "primary", []string{"fallback1"})

	job := &core.CronJob{
		ID:          "cron-1",
		Project:     "test-proj",
		SessionKey:  "telegram:123",
		Prompt:      "run check",
		SessionMode: "new_per_run",
		Enabled:     true,
	}

	err := eng.ExecuteCronJob(job)
	if err != nil {
		t.Fatalf("expected successful turn, got: %v", err)
	}
	if agent.turnIndex != 1 {
		t.Fatalf("expected exactly 1 turn execution, got %d", agent.turnIndex)
	}
}

// 2 & 5. Quota wall + fallback & retry succeeds
func TestCronScenarios_QuotaWallWithFallback_RetrySucceeds(t *testing.T) {
	agent := &fullMockHistoryAgent{
		providers: []core.ProviderConfig{
			{Name: "primary"},
			{Name: "fallback1"},
		},
		activeIdx: 0,
		turnOutputs: []string{
			"You've hit your session limit · resets 10am (Europe/Paris)", // attempt 1: quota wall
			"Daily report from fallback: all green.",                    // attempt 2: retry succeeds
		},
	}
	eng := createTestEngine(agent, "primary", []string{"fallback1"})

	job := &core.CronJob{
		ID:          "cron-2",
		Project:     "test-proj",
		SessionKey:  "telegram:123",
		Prompt:      "run check",
		SessionMode: "new_per_run",
		Enabled:     true,
	}

	err := eng.ExecuteCronJob(job)
	if err != nil {
		t.Fatalf("expected failover retry to succeed, got: %v", err)
	}
	if agent.turnIndex != 2 {
		t.Fatalf("expected 2 turns executed (initial + retry), got %d", agent.turnIndex)
	}

	agent.mu.Lock()
	switches := append([]string(nil), agent.switchCalls...)
	agent.mu.Unlock()

	if len(switches) < 2 {
		t.Fatalf("expected reset + failover switch, got %v", switches)
	}
	if switches[0] != "primary" {
		t.Errorf("switch[0] = %q, want primary reset", switches[0])
	}
	if switches[len(switches)-1] != "fallback1" {
		t.Errorf("last switch = %q, want fallback1", switches[len(switches)-1])
	}
}

// 3. Quota wall + no fallback -> hard failure
func TestCronScenarios_QuotaWall_NoFallback(t *testing.T) {
	agent := &fullMockHistoryAgent{
		providers: []core.ProviderConfig{
			{Name: "primary"},
		},
		activeIdx:   0,
		turnOutputs: []string{"You've hit your session limit · resets 10am"},
	}
	eng := createTestEngine(agent, "primary", nil) // no fallback configured

	job := &core.CronJob{
		ID:          "cron-3",
		Project:     "test-proj",
		SessionKey:  "telegram:123",
		Prompt:      "run check",
		SessionMode: "new_per_run",
		Enabled:     true,
	}

	err := eng.ExecuteCronJob(job)
	if err == nil {
		t.Fatalf("expected error for quota wall without fallback, got nil")
	}
	if !strings.Contains(err.Error(), "session quota limit reached") && !strings.Contains(err.Error(), "produced an empty response") {
		t.Fatalf("expected quota failure message, got: %v", err)
	}
}

// 4. Quota wall + exhausted/invalid fallback -> hard failure
func TestCronScenarios_QuotaWall_ExhaustedFallback(t *testing.T) {
	agent := &fullMockHistoryAgent{
		providers: []core.ProviderConfig{
			{Name: "primary"},
		},
		activeIdx:   0,
		turnOutputs: []string{"You have hit your session limit"},
	}
	// fallback list only contains the currently active provider
	eng := createTestEngine(agent, "primary", []string{"primary"})

	job := &core.CronJob{
		ID:          "cron-4",
		Project:     "test-proj",
		SessionKey:  "telegram:123",
		Prompt:      "run check",
		SessionMode: "new_per_run",
		Enabled:     true,
	}

	err := eng.ExecuteCronJob(job)
	if err == nil {
		t.Fatalf("expected error for exhausted fallback, got nil")
	}
}

// 6. Retry also fails -> stop and report error
func TestCronScenarios_RetryAlsoFails(t *testing.T) {
	agent := &fullMockHistoryAgent{
		providers: []core.ProviderConfig{
			{Name: "primary"},
			{Name: "fallback1"},
		},
		activeIdx: 0,
		turnOutputs: []string{
			"You've hit your session limit · resets 10am", // attempt 1
			"You've hit your session limit · resets 10am", // attempt 2 (retry also fails)
		},
	}
	eng := createTestEngine(agent, "primary", []string{"fallback1"})

	job := &core.CronJob{
		ID:          "cron-6",
		Project:     "test-proj",
		SessionKey:  "telegram:123",
		Prompt:      "run check",
		SessionMode: "new_per_run",
		Enabled:     true,
	}

	err := eng.ExecuteCronJob(job)
	if err == nil {
		t.Fatalf("expected error when retry also fails, got nil")
	}
	if agent.turnIndex != 2 {
		t.Fatalf("expected exactly 2 attempts (max 1 retry), got %d", agent.turnIndex)
	}
}

// 7. Next cron run resets to primary
func TestCronScenarios_NextRunResetsToPrimary(t *testing.T) {
	agent := &fullMockHistoryAgent{
		providers: []core.ProviderConfig{
			{Name: "primary"},
			{Name: "fallback1"},
		},
		activeIdx: 0,
		turnOutputs: []string{
			"You've hit your session limit · resets 10am", // run 1, attempt 1 -> failover to fallback1
			"Run 1 success on fallback1",                  // run 1, attempt 2
			"Run 2 success on primary",                    // run 2, attempt 1 (must reset to primary!)
		},
	}
	eng := createTestEngine(agent, "primary", []string{"fallback1"})

	job := &core.CronJob{
		ID:          "cron-7",
		Project:     "test-proj",
		SessionKey:  "telegram:123",
		Prompt:      "run check",
		SessionMode: "new_per_run",
		Enabled:     true,
	}

	// First cron run (triggers failover)
	if err := eng.ExecuteCronJob(job); err != nil {
		t.Fatalf("run 1 failed: %v", err)
	}

	// At end of run 1, active provider is fallback1
	if cur := agent.GetActiveProvider(); cur == nil || cur.Name != "fallback1" {
		t.Fatalf("after run 1, active provider should be fallback1, got %v", cur)
	}

	// Second cron run (must reset to primary!)
	if err := eng.ExecuteCronJob(job); err != nil {
		t.Fatalf("run 2 failed: %v", err)
	}

	agent.mu.Lock()
	switches := append([]string(nil), agent.switchCalls...)
	agent.mu.Unlock()

	// Sequence of switches:
	// 1. Run 1 start -> reset to "primary"
	// 2. Run 1 failover -> switch to "fallback1"
	// 3. Run 2 start -> reset to "primary"
	if len(switches) < 3 {
		t.Fatalf("expected at least 3 switches, got %v", switches)
	}
	if switches[2] != "primary" {
		t.Fatalf("expected run 2 to reset to primary, got %q", switches[2])
	}
}

// 8. Non-new_per_run behavior unchanged
func TestCronScenarios_NonNewPerRunBehaviorUnchanged(t *testing.T) {
	agent := &fullMockHistoryAgent{
		providers: []core.ProviderConfig{
			{Name: "primary"},
			{Name: "fallback1"},
		},
		activeIdx:   0,
		turnOutputs: []string{"Reuse session response"},
	}
	eng := createTestEngine(agent, "primary", []string{"fallback1"})

	job := &core.CronJob{
		ID:          "cron-8",
		Project:     "test-proj",
		SessionKey:  "telegram:123",
		Prompt:      "run check",
		SessionMode: "reuse",
		Enabled:     true,
	}

	err := eng.ExecuteCronJob(job)
	if err != nil {
		t.Fatalf("expected reuse session run to succeed, got: %v", err)
	}

	agent.mu.Lock()
	switches := len(agent.switchCalls)
	agent.mu.Unlock()

	// In reuse mode, primary provider reset does not run
	if switches != 0 {
		t.Fatalf("expected 0 switches in reuse mode, got %d", switches)
	}
}

// Evidence-based regression: validates the exact Claude quota wall specimen:
// "You've hit your session limit · resets 10am (Europe/Paris)"
// flowing through Nexus recovery decider and triggering:
// primary -> RETRY -> fallback -> fresh side session -> same cron job -> next run resets to primary.
func TestCronEvidenceBasedRegression_RealTranscriptShapeAndSpecimen(t *testing.T) {
	agent := &fullMockHistoryAgent{
		providers: []core.ProviderConfig{
			{Name: "primary-bedrock"},
			{Name: "fallback-minimax"},
		},
		activeIdx: 0,
		turnOutputs: []string{
			"You've hit your session limit · resets 10am (Europe/Paris)", // Run 1 Attempt 1 (Quota wall)
			"Successfully executed scheduled check on fallback provider.", // Run 1 Retry (Fallback success)
			"Successfully executed scheduled check on primary provider.",  // Run 2 Attempt 1 (Reset to primary)
		},
	}
	eng := createTestEngine(agent, "primary-bedrock", []string{"fallback-minimax"})

	job := &core.CronJob{
		ID:          "cron-evidence-1",
		Project:     "test-proj",
		SessionKey:  "telegram:123",
		Prompt:      "Standing Participation v1 scheduled check.",
		SessionMode: "new_per_run",
		Enabled:     true,
	}

	// 1. Run 1: initial attempt hits quota wall specimen, triggers failover to fallback-minimax in fresh side session
	if err := eng.ExecuteCronJob(job); err != nil {
		t.Fatalf("Run 1 expected to succeed via failover retry, got error: %v", err)
	}

	// Verify at end of Run 1 that 2 attempts were made and active provider is fallback-minimax
	if agent.turnIndex != 2 {
		t.Fatalf("Run 1 should have taken 2 turns (attempt 1 + retry), took %d", agent.turnIndex)
	}
	if cur := agent.GetActiveProvider(); cur == nil || cur.Name != "fallback-minimax" {
		t.Fatalf("After Run 1 failover, active provider should be fallback-minimax, got %v", cur)
	}

	// 2. Run 2: next cron run resets back to primary-bedrock before executing
	if err := eng.ExecuteCronJob(job); err != nil {
		t.Fatalf("Run 2 expected to succeed on primary, got error: %v", err)
	}

	agent.mu.Lock()
	switches := append([]string(nil), agent.switchCalls...)
	agent.mu.Unlock()

	// Verify exact switch sequence:
	// 1. Run 1 start: resets to primary-bedrock
	// 2. Run 1 failover: switches to fallback-minimax
	// 3. Run 2 start: resets to primary-bedrock
	if len(switches) < 3 {
		t.Fatalf("expected at least 3 provider switches, got %v", switches)
	}
	if switches[0] != "primary-bedrock" {
		t.Errorf("switch[0] = %q, want 'primary-bedrock'", switches[0])
	}
	if switches[1] != "fallback-minimax" {
		t.Errorf("switch[1] = %q, want 'fallback-minimax'", switches[1])
	}
	if switches[2] != "primary-bedrock" {
		t.Errorf("switch[2] = %q, want 'primary-bedrock'", switches[2])
	}
}

