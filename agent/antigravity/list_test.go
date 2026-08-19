package antigravity

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

const schema = "CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', preview TEXT NOT NULL DEFAULT '', step_count INTEGER NOT NULL DEFAULT 0, last_modified_time datetime, workspace_uris TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '', project_id TEXT NOT NULL DEFAULT '', agent_name TEXT NOT NULL DEFAULT '', parent_conversation_id TEXT NOT NULL DEFAULT '', nesting_depth INTEGER NOT NULL DEFAULT 0, battle_id TEXT NOT NULL DEFAULT '', winning_conversation_id TEXT NOT NULL DEFAULT '', not_fully_idle numeric NOT NULL DEFAULT false, killed numeric NOT NULL DEFAULT false, last_user_input_time datetime, last_user_input_step_index INTEGER NOT NULL DEFAULT -1, app_data_dir TEXT NOT NULL DEFAULT '')"

func TestListAntigravitySessions_WorkspaceScoped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	db := summariesDB(t, filepath.Join(base, "conversation_summaries.db"))
	defer db.Close()
	summary(t, db, "same", []string{"F:\\worlds\\world-nexus"})
	summary(t, db, "foreign", []string{"file:///f%253A/GitHub/resonova"})
	summary(t, db, "multi", []string{"file:///F:/GitHub/resonova", "file:///f%253A/worlds/world-nexus"})
	summary(t, db, "missing", nil)
	summary(t, db, "malformed", []string{"not-a-path"})
	for _, id := range []string{"same", "foreign", "multi", "missing", "malformed"} {
		transcript(t, base, id)
	}
	db.Close()
	got, err := listAntigravitySessions("F:\\worlds\\world-nexus")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range got {
		seen[s.ID] = true
	}
	for _, id := range []string{"same", "multi"} {
		if !seen[id] {
			t.Errorf("%s missing", id)
		}
	}
	for _, id := range []string{"foreign", "missing", "malformed"} {
		if seen[id] {
			t.Errorf("%s leaked", id)
		}
	}
}

func TestListAntigravitySessions_FallbackAndMalformed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	db := summariesDB(t, filepath.Join(base, "conversation_summaries.db"))
	defer db.Close()
	summary(t, db, "fallback", nil)
	fallbackConversationDB(t, base, "fallback", "file:///F:/worlds/world-nexus")
	conversationDB(t, base, "broken", nil)
	db.Close()
	got, err := listAntigravitySessions("F:\\worlds\\world-nexus")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "fallback" {
		t.Fatalf("got %#v", got)
	}
}

func TestDeleteSession_EnforcesOwnership(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	db := summariesDB(t, filepath.Join(base, "conversation_summaries.db"))
	defer db.Close()
	summary(t, db, "owned", []string{"file:///F:/worlds/world-nexus"})
	summary(t, db, "foreign", []string{"file:///F:/GitHub/resonova"})
	conversationDB(t, base, "owned", nil)
	conversationDB(t, base, "foreign", nil)
	transcript(t, base, "owned")
	transcript(t, base, "foreign")
	db.Close()
	a := &Agent{workDir: "F:\\worlds\\world-nexus"}
	if err := a.DeleteSession(context.Background(), "owned"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"foreign", "unknown"} {
		if err := a.DeleteSession(context.Background(), id); err == nil {
			t.Errorf("%s unexpectedly deleted", id)
		}
	}
}

func TestDeleteSession_RejectsIdentityMismatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	conversationDBWithIdentity(t, base, "requested", "stored", []string{"file:///F:/worlds/world-nexus"})
	a := &Agent{workDir: "F:\\worlds\\world-nexus"}
	if err := a.DeleteSession(context.Background(), "requested"); err == nil {
		t.Fatal("identity mismatch unexpectedly deleted")
	}
}

func summariesDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func summary(t *testing.T, db *sql.DB, id string, ws []string) {
	t.Helper()
	b, _ := json.Marshal(ws)
	if ws == nil {
		b = []byte("[]")
	}
	if _, err := db.Exec("INSERT INTO conversation_summaries (conversation_id, workspace_uris) VALUES (?, ?)", id, string(b)); err != nil {
		t.Fatal(err)
	}
}

func conversationDB(t *testing.T, base, id string, ws []string) {
	conversationDBWithIdentity(t, base, id, id, ws)
}

func conversationDBWithIdentity(t *testing.T, base, filenameID, storedID string, ws []string) {
	t.Helper()
	path := filepath.Join(base, "conversations", filenameID+".db")
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE trajectory_metadata_blob (id TEXT PRIMARY KEY, data BLOB)"); err != nil {
		t.Fatal(err)
	}
	if ws != nil {
		b, _ := json.Marshal(map[string]any{"conversation_id": storedID, "workspace_uris": ws})
		if _, err = db.Exec("INSERT INTO trajectory_metadata_blob (id, data) VALUES ('main', ?)", b); err != nil {
			t.Fatal(err)
		}
	}
}

func fallbackConversationDB(t *testing.T, base, id, workspaceURI string) {
	t.Helper()
	path := filepath.Join(base, "conversations", id+".db")
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE trajectory_metadata_blob (id TEXT PRIMARY KEY, data BLOB)"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO trajectory_metadata_blob (id, data) VALUES ('main', ?)", []byte("\x0a"+workspaceURI+"\x12ignored")); err != nil {
		t.Fatal(err)
	}
}

func transcript(t *testing.T, base, id string) {
	t.Helper()
	p := filepath.Join(base, "brain", id, ".system_generated", "logs", "transcript.jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0755)
	if err := os.WriteFile(p, []byte("{\"type\":\"USER_INPUT\",\"content\":\"prompt\"}\n"), 0644); err != nil {
		t.Fatal(err)
	}
}
