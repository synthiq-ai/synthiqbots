package saves

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleSavedVars = `
SynthiqBotsUISave = {
	["AutoChatCommands"] = "true",
	["MultiBarPoint"] = "10, 20",
	feedback_queue = {
		{
			id = "1745673600-4823",
			ts = 1745673600,
			note = "bot pulled twice while I was at 30%",
			screenshot_hint = "2026-04-26 14:00:03",
			chat_history = {
				{ ts = 1745673590, channel = "PARTY", sender = "Geek", text = "pulling" },
				{ ts = 1745673595, channel = "WHISPER", sender = "the operator", text = "wait healer oom" },
			},
			ctx = {
				char = "the operator-SynthiqEU",
				char_class = "PRIEST",
				char_lvl = 47,
				zone = "Stranglethorn Vale",
				subzone = "Grom'gol Base Camp",
				target = "Bloodscalp Hunter|Hunter|46|82",
				group = { "Geek", "Claude", "Clawd" },
				addon_ver = "2.0.0",
			},
			status = "pending",
		},
		{
			id = "1745673700-1129",
			ts = 1745673700,
			note = "",
			chat_history = {},
			ctx = {
				char = "the operator-SynthiqEU",
				char_class = "PRIEST",
				char_lvl = 47,
				zone = "Stranglethorn Vale",
			},
			status = "pending",
		},
	},
}
`

func TestParseHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SynthiqBotsUI.lua")
	if err := os.WriteFile(path, []byte(sampleSavedVars), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	e := entries[0]
	if e.ID != "1745673600-4823" {
		t.Errorf("id: %q", e.ID)
	}
	if e.AddonTs != 1745673600 {
		t.Errorf("ts: %d", e.AddonTs)
	}
	if e.Note != "bot pulled twice while I was at 30%" {
		t.Errorf("note: %q", e.Note)
	}
	if e.CharName != "the operator" {
		t.Errorf("charName: %q", e.CharName)
	}
	if e.CharRealm != "SynthiqEU" {
		t.Errorf("charRealm: %q", e.CharRealm)
	}
	if e.CharClass != "PRIEST" {
		t.Errorf("charClass: %q", e.CharClass)
	}
	if e.CharLvl != 47 {
		t.Errorf("charLvl: %d", e.CharLvl)
	}
	if e.Zone != "Stranglethorn Vale" {
		t.Errorf("zone: %q", e.Zone)
	}
	if len(e.ChatHistory) != 2 {
		t.Errorf("chatHistory: %d", len(e.ChatHistory))
	}
	if got := e.ChatHistory[0]["text"]; got != "pulling" {
		t.Errorf("first chat text: %v", got)
	}
	// Group should round-trip as a slice of strings inside ctx.
	if grp, ok := e.Ctx["group"].([]any); !ok || len(grp) != 3 {
		t.Errorf("group: %T %v", e.Ctx["group"], e.Ctx["group"])
	}

	// Second entry has empty note + ctx without optional fields.
	e2 := entries[1]
	if e2.ID != "1745673700-1129" {
		t.Errorf("id2: %q", e2.ID)
	}
	if e2.Note != "" {
		t.Errorf("note2: %q", e2.Note)
	}
	if e2.Zone != "Stranglethorn Vale" {
		t.Errorf("zone2: %q", e2.Zone)
	}
}

func TestParseMissingFile(t *testing.T) {
	dir := t.TempDir()
	entries, err := Parse(filepath.Join(dir, "does-not-exist.lua"))
	if err != nil {
		t.Fatalf("missing file should not be an error: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries, got %v", entries)
	}
}

func TestParseEmptyGlobal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.lua")
	if err := os.WriteFile(path, []byte("-- nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if entries != nil {
		t.Errorf("expected nil, got %v", entries)
	}
}

func TestParseNoFeedbackQueue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "noqueue.lua")
	if err := os.WriteFile(path, []byte(`SynthiqBotsUISave = { AutoChatCommands = "true" }`), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if entries != nil {
		t.Errorf("expected nil, got %v", entries)
	}
}

func TestParseSkipsEntryWithNoID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.lua")
	content := `
SynthiqBotsUISave = {
	feedback_queue = {
		{ ts = 1, note = "no id" },
		{ id = "valid-1", ts = 2 },
	},
}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry (skip the malformed row), got %d", len(entries))
	}
	if entries[0].ID != "valid-1" {
		t.Errorf("id: %q", entries[0].ID)
	}
}

func TestSplitCharRealm(t *testing.T) {
	cases := []struct {
		in, name, realm string
	}{
		{"the operator-SynthiqEU", "the operator", "SynthiqEU"},
		{"Char-With-Hyphens-Realm", "Char-With-Hyphens", "Realm"},
		{"NoRealm", "NoRealm", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		gotName, gotRealm := splitCharRealm(c.in)
		if gotName != c.name || gotRealm != c.realm {
			t.Errorf("splitCharRealm(%q) = (%q, %q), want (%q, %q)",
				c.in, gotName, gotRealm, c.name, c.realm)
		}
	}
}
