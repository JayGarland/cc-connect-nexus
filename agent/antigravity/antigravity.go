package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("antigravity", New)
}

// Agent drives the Antigravity CLI (agy) in headless mode.
//
// Modes (maps to agy approval and sandbox flags):
//   - "default":   ask for each tool through the cc-connect permission bridge
//   - "yolo":      auto-approve all tools (--dangerously-skip-permissions)
//   - "plan":      read-only plan mode with terminal sandbox constraints (--sandbox)
type Agent struct {
	workDir      string
	model        string
	mode         string
	cmd          string   // CLI binary name, default "agy"
	cliExtraArgs []string // extra args from cmd after the binary name
	configEnv    []string // env vars from [projects.agent.options.env]
	timeout      time.Duration
	providers    []core.ProviderConfig
	activeIdx    int
	sessionEnv   []string
	mu           sync.RWMutex
}

func New(opts map[string]any) (core.Agent, error) {
	workDir, _ := opts["work_dir"].(string)
	if workDir == "" {
		workDir = "."
	}
	model, _ := opts["model"].(string)
	mode, _ := opts["mode"].(string)
	mode = normalizeMode(mode)
	cmd, extraArgs := core.ParseCmdOpts(opts, "agy")

	var timeoutMins int64
	switch v := opts["timeout_mins"].(type) {
	case int64:
		timeoutMins = v
	case int:
		timeoutMins = int64(v)
	case float64:
		timeoutMins = int64(v)
	default:
		if v != nil {
			slog.Debug("antigravity: timeout_mins has unexpected type", "type", fmt.Sprintf("%T", v))
		}
	}
	var timeout time.Duration
	if timeoutMins > 0 {
		timeout = time.Duration(timeoutMins) * time.Minute
	}

	if _, err := exec.LookPath(cmd); err != nil {
		return nil, fmt.Errorf("antigravity: %q CLI not found in PATH, install from: https://antigravity.google/docs/cli-overview", cmd)
	}

	return &Agent{
		workDir:      workDir,
		model:        model,
		mode:         mode,
		cmd:          cmd,
		cliExtraArgs: extraArgs,
		configEnv:    core.ParseConfigEnv(opts),
		timeout:      timeout,
		activeIdx:    -1,
	}, nil
}

func normalizeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "yolo", "auto", "force", "bypasspermissions":
		return "yolo"
	case "plan", "sandbox":
		return "plan"
	default:
		return "default"
	}
}

func (a *Agent) Name() string { return "antigravity" }

func (a *Agent) SetWorkDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workDir = dir
	slog.Info("antigravity: work_dir changed", "work_dir", dir)
}

func (a *Agent) GetWorkDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workDir
}

func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = model
	slog.Info("antigravity: model changed", "model", model)
}

func (a *Agent) GetModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return core.GetProviderModel(a.providers, a.activeIdx, a.model)
}

func (a *Agent) configuredModels() []core.ModelOption {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return core.GetProviderModels(a.providers, a.activeIdx)
}

func (a *Agent) AvailableModels(ctx context.Context) []core.ModelOption {
	if models := a.configuredModels(); len(models) > 0 {
		return models
	}
	if models := a.fetchModelsFromAPI(ctx); len(models) > 0 {
		return models
	}
	return []core.ModelOption{
		{Name: "gemini-3.1-pro-preview", Desc: "Gemini 3.1 Pro Preview"},
		{Name: "gemini-3-flash-preview", Desc: "Gemini 3 Flash Preview"},
		{Name: "gemini-2.5-pro", Desc: "Gemini 2.5 Pro"},
		{Name: "gemini-2.5-flash", Desc: "Gemini 2.5 Flash"},
	}
}

func (a *Agent) fetchModelsFromAPI(ctx context.Context) []core.ModelOption {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("GOOGLE_API_KEY")
	}
	if apiKey == "" {
		return nil
	}

	url := "https://generativelanguage.googleapis.com/v1beta/models?key=" + apiKey
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Debug("antigravity: failed to fetch models", "error", err)
		return nil
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	var models []core.ModelOption
	for _, m := range result.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		if !strings.HasPrefix(id, "gemini-") {
			continue
		}
		models = append(models, core.ModelOption{Name: id, Desc: m.DisplayName})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name > models[j].Name })
	return models
}

func (a *Agent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionEnv = env
}

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	model := a.model
	mode := a.mode
	cmd := a.cmd
	extraArgs := append([]string{}, a.cliExtraArgs...)
	workDir := a.workDir
	timeout := a.timeout
	extraEnv := append([]string(nil), a.configEnv...)
	extraEnv = append(extraEnv, a.providerEnvLocked()...)
	extraEnv = append(extraEnv, a.sessionEnv...)
	if a.activeIdx >= 0 && a.activeIdx < len(a.providers) {
		if m := a.providers[a.activeIdx].Model; m != "" {
			model = m
		}
	}
	a.mu.Unlock()

	return newAntigravitySession(ctx, cmd, extraArgs, workDir, model, mode, sessionID, extraEnv, timeout)
}

func (a *Agent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	return listAntigravitySessions(a.workDir)
}

func (a *Agent) DeleteSession(_ context.Context, sessionID string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("antigravity: cannot determine home dir: %w", err)
	}
	baseDir := filepath.Join(homeDir, ".gemini", "antigravity-cli")
	record, ok := findConversationRecord(baseDir, sessionID)
	if !ok || record.ID != sessionID {
		return fmt.Errorf("antigravity: session not found or identity mismatch: %s", sessionID)
	}
	if !workspaceMatches(a.workDir, record.Workspaces) {
		return fmt.Errorf("antigravity: session %s belongs to another workDir", sessionID)
	}
	brainPath := filepath.Join(baseDir, "brain", sessionID)
	dbPath := filepath.Join(baseDir, "conversations", sessionID+".db")
	removed := false
	if _, err := os.Stat(brainPath); err == nil {
		if err := os.RemoveAll(brainPath); err != nil {
			return fmt.Errorf("antigravity: remove brain: %w", err)
		}
		removed = true
	}
	if _, err := os.Stat(dbPath); err == nil {
		if err := os.Remove(dbPath); err != nil {
			return fmt.Errorf("antigravity: remove conversation: %w", err)
		}
		removed = true
	}
	if !removed {
		return fmt.Errorf("session file not found: %s", sessionID)
	}
	return nil
}

func (a *Agent) Stop() error { return nil }

func (a *Agent) SetMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = normalizeMode(mode)
	slog.Info("antigravity: mode changed", "mode", a.mode)
}

func (a *Agent) GetMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

func (a *Agent) PermissionModes() []core.PermissionModeInfo {
	return []core.PermissionModeInfo{
		{Key: "default", Name: "Default", NameZh: "默认", Desc: "Ask for approval through cc-connect on each tool use", DescZh: "每次工具调用都通过 cc-connect 请求确认"},
		{Key: "yolo", Name: "YOLO", NameZh: "全自动", Desc: "Auto-approve all tool calls", DescZh: "自动批准所有工具调用"},
		{Key: "plan", Name: "Plan", NameZh: "规划模式", Desc: "Read-only plan mode in sandbox", DescZh: "只读沙箱规划模式"},
	}
}

func (a *Agent) CommandDirs() []string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	dirs := []string{filepath.Join(absDir, ".gemini", "commands")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".gemini", "commands"))
	}
	return dirs
}

func (a *Agent) SkillDirs() []string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	dirs := []string{filepath.Join(absDir, ".gemini", "skills")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".gemini", "skills"))
	}
	return dirs
}

func (a *Agent) CompressCommand() string { return "" }

func (a *Agent) ProjectMemoryFile() string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	return filepath.Join(absDir, "GEMINI.md")
}

func (a *Agent) GlobalMemoryFile() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(homeDir, ".gemini", "GEMINI.md")
}

func (a *Agent) SetProviders(providers []core.ProviderConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.providers = providers
}

func (a *Agent) SetActiveProvider(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if name == "" {
		a.activeIdx = -1
		slog.Info("antigravity: provider cleared")
		return true
	}
	for i, p := range a.providers {
		if p.Name == name {
			a.activeIdx = i
			slog.Info("antigravity: provider switched", "provider", name)
			return true
		}
	}
	return false
}

func (a *Agent) GetActiveProvider() *core.ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeIdx < 0 || a.activeIdx >= len(a.providers) {
		return nil
	}
	p := a.providers[a.activeIdx]
	return &p
}

func (a *Agent) ListProviders() []core.ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]core.ProviderConfig, len(a.providers))
	copy(result, a.providers)
	return result
}

func (a *Agent) providerEnvLocked() []string {
	if a.activeIdx < 0 || a.activeIdx >= len(a.providers) {
		return nil
	}
	p := a.providers[a.activeIdx]
	var env []string
	if p.APIKey != "" {
		env = append(env, "GEMINI_API_KEY="+p.APIKey)
	}
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func antigravityProjectSlug(workDir string) string {
	abs, err := filepath.Abs(workDir)
	if err != nil {
		abs = workDir
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return slugify(filepath.Base(abs))
	}

	data, err := os.ReadFile(filepath.Join(homeDir, ".gemini", "projects.json"))
	if err == nil {
		var registry struct {
			Projects map[string]string `json:"projects"`
		}
		if json.Unmarshal(data, &registry) == nil {
			normalized := filepath.Clean(abs)
			if slug, ok := registry.Projects[normalized]; ok {
				return slug
			}
		}
	}

	return slugify(filepath.Base(abs))
}

func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	result := b.String()
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	result = strings.Trim(result, "-")
	if result == "" {
		result = "project"
	}
	return result
}

func listAntigravitySessions(workDir string) ([]core.AgentSessionInfo, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("antigravity: cannot determine home dir: %w", err)
	}

	baseDir := filepath.Join(homeDir, ".gemini", "antigravity-cli")
	records := discoverConversationRecords(baseDir)
	var sessions []core.AgentSessionInfo
	for _, record := range records {
		if !workspaceMatches(workDir, record.Workspaces) {
			continue
		}
		sessionID := record.ID
		if strings.TrimSpace(sessionID) == "" {
			continue
		}
		brainDir := filepath.Join(baseDir, "brain")
		transcriptPath := filepath.Join(brainDir, sessionID, ".system_generated", "logs", "transcript.jsonl")
		info, err := os.Stat(transcriptPath)
		if err != nil {
			dbPath := filepath.Join(baseDir, "conversations", sessionID+".db")
			dbInfo, dbErr := os.Stat(dbPath)
			if dbErr != nil {
				continue
			}
			info = dbInfo
		}

		summary, msgCount := extractTranscriptInfo(transcriptPath)
		if summary == "" {
			summary = sessionID
		}
		if utf8.RuneCountInString(summary) > 60 {
			summary = string([]rune(summary)[:60]) + "..."
		}

		sessions = append(sessions, core.AgentSessionInfo{
			ID:           sessionID,
			Summary:      summary,
			MessageCount: msgCount,
			ModifiedAt:   info.ModTime(),
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt)
	})

	return sessions, nil
}

type conversationRecord struct {
	ID         string
	Workspaces []string
	ModifiedAt time.Time
}

func discoverConversationRecords(baseDir string) []conversationRecord {
	records := make(map[string]conversationRecord)
	summaryDB := filepath.Join(baseDir, "conversation_summaries.db")
	if db, err := sql.Open("sqlite", "file:"+summaryDB+"?mode=ro"); err == nil {
		rows, err := db.Query("SELECT conversation_id, workspace_uris, last_modified_time FROM conversation_summaries")
		if err == nil {
			for rows.Next() {
				var id, raw string
				var modified sql.NullString
				if rows.Scan(&id, &raw, &modified) != nil || strings.TrimSpace(id) == "" {
					continue
				}
				workspaces := parseWorkspaceURIs(raw)
				if len(workspaces) == 0 {
					continue
				}
				records[id] = conversationRecord{ID: id, Workspaces: workspaces, ModifiedAt: parseTime(modified.String)}
			}
			_ = rows.Close()
		}
		_ = db.Close()
	}
	convDir := filepath.Join(baseDir, "conversations")
	entries, _ := os.ReadDir(convDir)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".db")
		if _, exists := records[id]; exists {
			continue
		}
		if record, ok := readConversationRecord(filepath.Join(convDir, entry.Name()), id); ok {
			records[id] = record
		}
	}
	result := make([]conversationRecord, 0, len(records))
	for _, record := range records {
		result = append(result, record)
	}
	return result
}

func findConversationRecord(baseDir, id string) (conversationRecord, bool) {
	for _, record := range discoverConversationRecords(baseDir) {
		if record.ID == id {
			return record, true
		}
	}
	return conversationRecord{}, false
}

func readConversationRecord(path, filenameID string) (conversationRecord, bool) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return conversationRecord{}, false
	}
	defer db.Close()
	var blob []byte
	if err := db.QueryRow("SELECT data FROM trajectory_metadata_blob WHERE id='main'").Scan(&blob); err != nil {
		return conversationRecord{}, false
	}
	var meta struct {
		ConversationID string   `json:"conversation_id"`
		SessionID      string   `json:"session_id"`
		WorkspaceURIs  []string `json:"workspace_uris"`
	}
	if json.Unmarshal(blob, &meta) == nil {
		id := meta.ConversationID
		if id == "" {
			id = meta.SessionID
		}
		if id == "" {
			id = filenameID
		}
		if id != filenameID || len(meta.WorkspaceURIs) == 0 {
			return conversationRecord{}, false
		}
		return conversationRecord{ID: id, Workspaces: normalizeWorkspaceURIs(meta.WorkspaceURIs)}, true
	}
	workspaces := extractWorkspaceURIs(blob)
	if len(workspaces) == 0 {
		return conversationRecord{}, false
	}
	return conversationRecord{ID: filenameID, Workspaces: workspaces}, true
}

func parseWorkspaceURIs(raw string) []string {
	var values []string
	if json.Unmarshal([]byte(raw), &values) != nil {
		return nil
	}
	return normalizeWorkspaceURIs(values)
}
func normalizeWorkspaceURIs(values []string) []string {
	var result []string
	for _, value := range values {
		if path := canonicalWorkspacePath(value); path != "" {
			result = append(result, path)
		}
	}
	return result
}
func workspaceMatches(workDir string, values []string) bool {
	target := canonicalWorkspacePath(workDir)
	if target == "" {
		return false
	}
	for _, value := range values {
		if canonicalWorkspacePath(value) == target {
			return true
		}
	}
	return false
}

func canonicalWorkspacePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for strings.Contains(value, "%") {
		decoded, err := url.QueryUnescape(value)
		if err != nil || decoded == value {
			break
		}
		value = decoded
	}
	if strings.HasPrefix(strings.ToLower(value), "file://") {
		value = strings.TrimPrefix(value, "file://")
		value = strings.TrimPrefix(value, "/")
	}
	value = filepath.Clean(filepath.FromSlash(value))
	abs, err := filepath.Abs(value)
	if err == nil {
		value = abs
	}
	return strings.ToLower(value)
}

func parseTime(value string) time.Time { t, _ := time.Parse(time.RFC3339Nano, value); return t }
func extractWorkspaceURIs(blob []byte) []string {
	var found []string
	for _, match := range regexp.MustCompile(`(?i)file:///[^\x00-\x1f"']+`).FindAllString(string(blob), -1) {
		found = append(found, match)
	}
	return normalizeWorkspaceURIs(found)
}

func extractTranscriptInfo(transcriptPath string) (summary string, msgCount int) {
	file, err := os.Open(transcriptPath)
	if err != nil {
		return "", 0
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		msgCount++

		if summary == "" {
			var step struct {
				Type    string `json:"type"`
				Content string `json:"content"`
			}
			if json.Unmarshal(line, &step) == nil && step.Type == "USER_INPUT" && step.Content != "" {
				text := step.Content
				if strings.Contains(text, "<USER_REQUEST>") && strings.Contains(text, "</USER_REQUEST>") {
					parts := strings.Split(text, "<USER_REQUEST>")
					if len(parts) > 1 {
						subParts := strings.Split(parts[1], "</USER_REQUEST>")
						text = subParts[0]
					}
				}
				for _, lineStr := range strings.Split(text, "\n") {
					lineStr = strings.TrimSpace(lineStr)
					if lineStr != "" {
						summary = lineStr
						break
					}
				}
			}
		}
	}
	return summary, msgCount
}
