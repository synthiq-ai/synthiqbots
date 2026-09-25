package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkDirFS creates an empty file at path inside root, MkdirAll'ing parents.
// Used to build module-tree fixtures for walkModuleSQL.
func mkDirFS(t *testing.T, root, rel string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir parents %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte("-- test fixture\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

// TestWalkModuleSQL_ModOllamaChatShape mirrors mod-ollama-chat's actual
// data/sql/characters/{base,updates}/ tree. All files should land in
// Standard, none in Optional or Unrecognized.
func TestWalkModuleSQL_ModOllamaChatShape(t *testing.T) {
	root := t.TempDir()
	mkDirFS(t, root, "data/sql/characters/base/2026_04_26_admin_audit.sql")
	mkDirFS(t, root, "data/sql/characters/base/2026_04_26_feedback.sql")
	mkDirFS(t, root, "data/sql/characters/updates/2026_05_01_personality.sql")

	plan := walkModuleSQL(root)
	if len(plan.Standard) != 2 {
		t.Fatalf("standard buckets: got %d, want 2", len(plan.Standard))
	}
	for _, b := range plan.Standard {
		if b.Database != "acore_characters" {
			t.Errorf("expected acore_characters, got %s for %s", b.Database, b.RelDir)
		}
	}
	if len(plan.Optional) != 0 {
		t.Errorf("optional should be empty: %+v", plan.Optional)
	}
	if len(plan.Unrecognized) != 0 {
		t.Errorf("unrecognized should be empty: %+v", plan.Unrecognized)
	}
}

// TestWalkModuleSQL_ModIndividualProgressionShape mirrors the
// mod-individual-progression tree:
//
//	data/sql/{auth,characters,world}/{base,updates}/*.sql  (Standard)
//	optional/sql/world/zz_optional_phasing.sql             (Optional, "/optional/")
//	optional/sql/world/zz_optional_aq_quest_nerf.sql       (Optional, "/optional/")
//
// Both classification AND DB inference for optional must be correct (.../sql/world/...)
// → acore_world.
func TestWalkModuleSQL_ModIndividualProgressionShape(t *testing.T) {
	root := t.TempDir()
	mkDirFS(t, root, "data/sql/auth/base/2026_01_initial.sql")
	mkDirFS(t, root, "data/sql/characters/updates/2026_03_patch.sql")
	mkDirFS(t, root, "data/sql/world/base/zone_redridge.sql")
	mkDirFS(t, root, "data/sql/world/updates/2026_04_world_fix.sql")
	mkDirFS(t, root, "optional/sql/world/zz_optional_phasing.sql")
	mkDirFS(t, root, "optional/sql/world/zz_optional_aq_quest_nerf.sql")

	plan := walkModuleSQL(root)
	if len(plan.Standard) != 4 {
		t.Errorf("standard buckets: got %d, want 4", len(plan.Standard))
	}
	if len(plan.Optional) != 2 {
		t.Fatalf("optional: got %d, want 2", len(plan.Optional))
	}
	for _, opt := range plan.Optional {
		if opt.Database != "acore_world" {
			t.Errorf("DB inference for %s wrong: got %s, want acore_world", opt.RelPath, opt.Database)
		}
		if opt.Reason != "under /optional/" {
			t.Errorf("reason for %s: got %q", opt.RelPath, opt.Reason)
		}
	}
	if len(plan.Unrecognized) != 0 {
		t.Errorf("unrecognized should be empty: %+v", plan.Unrecognized)
	}
}

// TestWalkModuleSQL_ModPlayerbotsShape mirrors the mod-playerbots tree, which
// includes its own non-standard `playerbots` database AND custom/archive
// gameplay-affecting subdirs:
//
//	data/sql/world/{base,updates}/                  (Standard, acore_world)
//	data/sql/characters/{base,updates}/             (Standard, acore_characters)
//	data/sql/playerbots/{base,updates,custom,archive}/  (Unrecognized)
//
// Per design: playerbots/ goes to Unrecognized (the agent decides; we have
// no enum for that DB). custom/ and archive/ inside Unrecognized stay
// unrecognized — we don't promote them to Optional once the parent DB is
// already unmapped.
func TestWalkModuleSQL_ModPlayerbotsShape(t *testing.T) {
	root := t.TempDir()
	mkDirFS(t, root, "data/sql/world/base/init.sql")
	mkDirFS(t, root, "data/sql/world/updates/2026_05_pb.sql")
	mkDirFS(t, root, "data/sql/characters/base/init.sql")
	mkDirFS(t, root, "data/sql/playerbots/base/init.sql")
	mkDirFS(t, root, "data/sql/playerbots/custom/x.sql")
	mkDirFS(t, root, "data/sql/playerbots/archive/old.sql")

	plan := walkModuleSQL(root)
	// 3 standard buckets: world/base, world/updates, characters/base.
	if len(plan.Standard) != 3 {
		t.Errorf("standard: got %d, want 3", len(plan.Standard))
	}
	// playerbots/ surfaces ONCE in Unrecognized (the whole subtree under it
	// is folded into one entry — agent doesn't need per-file enumeration to
	// know it can't import unknown-DB SQL).
	if len(plan.Unrecognized) != 1 {
		t.Fatalf("unrecognized: got %d, want 1\n%+v", len(plan.Unrecognized), plan.Unrecognized)
	}
	if !strings.Contains(plan.Unrecognized[0].Reason, "playerbots") {
		t.Errorf("reason should name the unknown DB: %q", plan.Unrecognized[0].Reason)
	}
}

// TestWalkModuleSQL_ModAhBotLegacyDBWorld covers the mod-ah-bot quirk: SQL
// goes into data/sql/db-world/ instead of data/sql/world/{base,updates}/.
// We map db-world → acore_world via moduleSQLStandardDirs and the lone
// .sql files at that level (no base/updates split) are aggregated into a
// single Standard bucket.
func TestWalkModuleSQL_ModAhBotLegacyDBWorld(t *testing.T) {
	root := t.TempDir()
	mkDirFS(t, root, "data/sql/db-world/auctionhousebot.sql")
	mkDirFS(t, root, "data/sql/db-world/2026_03_ah.sql")

	plan := walkModuleSQL(root)
	if len(plan.Standard) != 1 {
		t.Fatalf("standard: got %d, want 1", len(plan.Standard))
	}
	b := plan.Standard[0]
	if b.Database != "acore_world" {
		t.Errorf("db-world should map to acore_world, got %s", b.Database)
	}
	if len(b.Files) != 2 {
		t.Errorf("files: got %d, want 2", len(b.Files))
	}
	// Files are sorted alphabetically — date-prefix migrations rely on this.
	if b.Files[0] != "2026_03_ah.sql" || b.Files[1] != "auctionhousebot.sql" {
		t.Errorf("files not sorted: %v", b.Files)
	}
}

// TestWalkModuleSQL_ZzOptionalInsideStandardTree covers a hybrid: a module
// puts a zz_optional_*.sql file alongside regular updates inside
// data/sql/world/updates/. It must surface as Optional (with reason
// "zz_optional_ prefix"), NOT auto-imported.
//
// Pin reflects the broader rule: optional detection is a UNION (any one of:
// path under /optional/, parent dir custom/archive/experimental, filename
// matches zz_optional_*) — so a module that ships an opt-in file inside the
// standard tree is still gated.
func TestWalkModuleSQL_ZzOptionalInsideStandardTree(t *testing.T) {
	root := t.TempDir()
	mkDirFS(t, root, "data/sql/world/updates/2026_05_world.sql")
	mkDirFS(t, root, "data/sql/world/updates/zz_optional_hardcore.sql")

	plan := walkModuleSQL(root)
	if len(plan.Standard) != 1 {
		t.Fatalf("standard buckets: got %d, want 1", len(plan.Standard))
	}
	// Both files end up in the Standard files list because they're inside
	// data/sql/world/updates/ — but the importer at runtime applies
	// isOptionalFilename() to elevate zz_optional_* files at *import* time.
	// (Walker leaves the bucket alone; importer filters.)
	hasZz := false
	for _, f := range plan.Standard[0].Files {
		if isOptionalFilename(f) {
			hasZz = true
		}
	}
	if !hasZz {
		t.Error("zz_optional_ file should be present in bucket files for filtering at import time")
	}
}

// TestWalkModuleSQL_NoSqlAtAll covers a module without any data/sql/ tree —
// the AzerothCore CMakeLists/PCH placeholders, etc. Empty plan, no error.
func TestWalkModuleSQL_NoSqlAtAll(t *testing.T) {
	root := t.TempDir()
	mkDirFS(t, root, "src/some_source.cpp")
	plan := walkModuleSQL(root)
	if len(plan.Standard)+len(plan.Optional)+len(plan.Unrecognized) != 0 {
		t.Errorf("empty module should yield empty plan: %+v", plan)
	}
}
