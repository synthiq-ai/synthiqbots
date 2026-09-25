package mcpserver

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ModuleSQLPlan is the discovery output for a single AzerothCore module —
// produced by walkModuleSQL and consumed by git_pull_module / git_pull_all
// when the caller passes import_sql:true. The three buckets correspond to
// the three policy outcomes:
//
//	Standard    — auto-import without asking. The standard tree shape is
//	              data/sql/{auth,characters,world}/{base,updates}/*.sql, plus
//	              the legacy aliases db-{auth,characters,world}/ that
//	              mod-ah-bot still uses. Any file under one of these dirs is
//	              safe to import in dependency order (alphabetical sort
//	              preserves AzerothCore's date-prefix convention).
//	Optional    — list back to the caller, import only if the agent named
//	              the file in optional_includes. Triggered by any of:
//	              path containing /optional/, filename matching
//	              zz_optional_*.sql, parent dir named custom/archive/experimental.
//	              These can affect or break gameplay (DBC-coupled phasing,
//	              stat curves, vendor changes) and must be opt-in.
//	Unrecognized — paths under data/sql/ that don't match any rule above
//	              (e.g. mod-playerbots' data/sql/playerbots/* targets a
//	              non-acore_* database we don't have an enum for). Reported
//	              so the caller can decide; never auto-imported.
type ModuleSQLPlan struct {
	Standard     []ModuleSQLDir   `json:"standard"`
	Optional     []ModuleSQLFile  `json:"optional_available"`
	Unrecognized []ModuleSQLEntry `json:"unrecognized"`
}

// ModuleSQLDir is one (database, dir) bucket of standard files. Files are
// pre-sorted alphabetically — same convention sql_import_dir uses.
type ModuleSQLDir struct {
	RelDir   string   `json:"rel_dir"`  // e.g. "data/sql/world/base"
	Database string   `json:"database"` // acore_world / acore_characters / acore_auth
	Files    []string `json:"files"`    // basenames only, alphabetical
}

// ModuleSQLFile is one optional file the agent must explicitly opt in to.
// `Database` is a best guess from the parent path (world/auth/characters
// chunk in the path); empty if we can't tell.
type ModuleSQLFile struct {
	RelPath  string `json:"rel_path"` // e.g. "optional/sql/world/zz_optional_phasing.sql"
	Basename string `json:"basename"` // for include-list lookups
	Database string `json:"database,omitempty"`
	Reason   string `json:"reason"` // why it landed in optional (e.g. "under /optional/")
}

// ModuleSQLEntry is a path we found under data/sql/ but couldn't map. The
// agent decides whether to skip, configure manually, or extend the walker.
type ModuleSQLEntry struct {
	RelPath string `json:"rel_path"`
	Reason  string `json:"reason"`
}

// Standard tree mapping. Keys are the immediate child directory of data/sql/
// in the canonical layout; values are the AzerothCore database name.
var moduleSQLStandardDirs = map[string]string{
	"world":         "acore_world",
	"characters":    "acore_characters",
	"auth":          "acore_auth",
	"db-world":      "acore_world",      // mod-ah-bot legacy
	"db-characters": "acore_characters", // theoretical, not seen in the wild but cheap to support
	"db-auth":       "acore_auth",
	"db_world":      "acore_world",
	"db_characters": "acore_characters",
	"db_auth":       "acore_auth",
}

// Standard subdirs UNDER {world,characters,auth}/ that auto-import. base
// holds initial DDL + reference data; updates holds incremental migrations
// named YYYY_MM_DD_NN.sql. Any other subdir (e.g. world/optional/, world/custom/)
// is routed to Optional/Unrecognized by walkModuleSQL.
var moduleSQLStandardSubdirs = map[string]bool{
	"base":    true,
	"updates": true,
}

// Names of subdirs that, anywhere in a module's data/sql/ subtree, mark every
// .sql under them as Optional. Captures the conventions across the modules we
// run: optional/ (mod-individual-progression), custom/ + archive/
// (mod-playerbots), experimental/ (general practice).
var moduleSQLOptionalDirs = map[string]bool{
	"optional":     true,
	"custom":       true,
	"archive":      true,
	"experimental": true,
}

// walkModuleSQL discovers SQL under modulePath/data/sql and modulePath/optional
// and partitions every .sql into Standard / Optional / Unrecognized buckets
// per the rules documented on ModuleSQLPlan. Pure function — no DB access,
// no os.Exec; safe to unit test against a fixture tree.
//
// modulePath is the absolute filesystem path to the module root (e.g.
// /wow-root/modules/mod-individual-progression). Missing dirs are silently
// treated as empty — a module without any SQL returns an empty plan rather
// than an error.
func walkModuleSQL(modulePath string) ModuleSQLPlan {
	plan := ModuleSQLPlan{
		Standard:     []ModuleSQLDir{},
		Optional:     []ModuleSQLFile{},
		Unrecognized: []ModuleSQLEntry{},
	}
	walkDataSQL(modulePath, &plan)
	walkOptionalRoot(modulePath, &plan)
	// Stable sort — agent output and golden tests need deterministic order.
	sort.SliceStable(plan.Standard, func(i, j int) bool {
		return plan.Standard[i].RelDir < plan.Standard[j].RelDir
	})
	sort.SliceStable(plan.Optional, func(i, j int) bool {
		return plan.Optional[i].RelPath < plan.Optional[j].RelPath
	})
	sort.SliceStable(plan.Unrecognized, func(i, j int) bool {
		return plan.Unrecognized[i].RelPath < plan.Unrecognized[j].RelPath
	})
	return plan
}

// walkDataSQL handles the canonical data/sql/ tree.
func walkDataSQL(modulePath string, plan *ModuleSQLPlan) {
	dataSQL := filepath.Join(modulePath, "data", "sql")
	dataEntries, err := os.ReadDir(dataSQL)
	if err != nil {
		return // no data/sql/ dir is normal for SQL-free modules
	}
	for _, dbDirEntry := range dataEntries {
		if !dbDirEntry.IsDir() {
			continue
		}
		dbName := dbDirEntry.Name()
		dbPath := filepath.Join(dataSQL, dbName)
		mappedDB, dbKnown := moduleSQLStandardDirs[dbName]

		if !dbKnown {
			plan.Unrecognized = append(plan.Unrecognized, ModuleSQLEntry{
				RelPath: filepath.Join("data", "sql", dbName) + "/",
				Reason:  "unknown database '" + dbName + "' (not in {world, characters, auth} or aliases)",
			})
			continue
		}
		// Walk subdirs under data/sql/<db>/.
		subEntries, err := os.ReadDir(dbPath)
		if err != nil {
			continue
		}
		for _, sub := range subEntries {
			if !sub.IsDir() {
				// .sql at data/sql/<db>/<file>.sql with no base/updates split — happens for db-world/
				if strings.HasSuffix(strings.ToLower(sub.Name()), ".sql") {
					addSQLAtPath(plan, dataSQL, sub.Name(), dbName, mappedDB, "")
				}
				continue
			}
			subName := sub.Name()
			subPath := filepath.Join(dbPath, subName)
			isOptional := moduleSQLOptionalDirs[subName]
			isStandard := moduleSQLStandardSubdirs[subName]

			files, err := listSQLFiles(subPath)
			if err != nil {
				continue
			}
			relDir := filepath.Join("data", "sql", dbName, subName)

			switch {
			case isStandard:
				if len(files) > 0 {
					plan.Standard = append(plan.Standard, ModuleSQLDir{
						RelDir:   relDir,
						Database: mappedDB,
						Files:    files,
					})
				}
			case isOptional:
				for _, f := range files {
					plan.Optional = append(plan.Optional, ModuleSQLFile{
						RelPath:  filepath.Join(relDir, f),
						Basename: f,
						Database: mappedDB,
						Reason:   "under /" + subName + "/",
					})
				}
			default:
				// Unknown subdir under a known database, e.g. data/sql/world/foo/.
				for _, f := range files {
					plan.Unrecognized = append(plan.Unrecognized, ModuleSQLEntry{
						RelPath: filepath.Join(relDir, f),
						Reason:  "unknown subdir '" + subName + "' under data/sql/" + dbName + "/",
					})
				}
			}
		}
	}
}

// walkOptionalRoot handles the top-level optional/ tree (mod-individual-progression
// pattern: optional/sql/world/zz_optional_*.sql). Files under here are always
// Optional regardless of subdir convention.
func walkOptionalRoot(modulePath string, plan *ModuleSQLPlan) {
	optRoot := filepath.Join(modulePath, "optional", "sql")
	if _, err := os.Stat(optRoot); err != nil {
		return
	}
	_ = filepath.WalkDir(optRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".sql") {
			return nil
		}
		rel, err := filepath.Rel(modulePath, path)
		if err != nil {
			return nil
		}
		// Best-effort DB inference from the path: optional/sql/world/X.sql → acore_world.
		db := ""
		// Path components after "optional/sql/": first token is the db name if it matches.
		parts := strings.Split(filepath.ToSlash(rel), "/")
		// parts = ["optional", "sql", <dbName>?, ...rest]
		if len(parts) >= 3 {
			if mapped, ok := moduleSQLStandardDirs[parts[2]]; ok {
				db = mapped
			}
		}
		plan.Optional = append(plan.Optional, ModuleSQLFile{
			RelPath:  rel,
			Basename: d.Name(),
			Database: db,
			Reason:   "under /optional/",
		})
		return nil
	})
}

// listSQLFiles returns alphabetically-sorted basenames of *.sql files in dir.
// Optional-pattern (zz_optional_*) files inside otherwise-standard dirs are
// elevated to Optional via the caller — this helper just lists.
func listSQLFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".sql") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// isOptionalFilename returns true if the basename matches the zz_optional_*
// convention used by mod-individual-progression's optional/ tree (and a few
// other modules). Used by the import driver to elevate a file out of the
// standard sweep even when it sits under a base/ or updates/ dir.
func isOptionalFilename(name string) bool {
	return strings.HasPrefix(name, "zz_optional_")
}

// addSQLAtPath appends a single .sql file under data/sql/<db>/ with no
// base/updates split (the mod-ah-bot db-world/ case). It still respects
// zz_optional_ promotion.
func addSQLAtPath(plan *ModuleSQLPlan, dataSQLRoot, basename, dbDirName, mappedDB, _subdir string) {
	relDir := filepath.Join("data", "sql", dbDirName)
	if isOptionalFilename(basename) {
		plan.Optional = append(plan.Optional, ModuleSQLFile{
			RelPath:  filepath.Join(relDir, basename),
			Basename: basename,
			Database: mappedDB,
			Reason:   "zz_optional_ prefix",
		})
		return
	}
	// Aggregate into a single "<dbDirName>" Standard entry — caller-side
	// dedupe by RelDir would be cleaner but per-call this keeps walkModuleSQL
	// linear-time without buffering the whole module first.
	for i := range plan.Standard {
		if plan.Standard[i].RelDir == relDir {
			plan.Standard[i].Files = append(plan.Standard[i].Files, basename)
			sort.Strings(plan.Standard[i].Files)
			return
		}
	}
	plan.Standard = append(plan.Standard, ModuleSQLDir{
		RelDir:   relDir,
		Database: mappedDB,
		Files:    []string{basename},
	})
}
