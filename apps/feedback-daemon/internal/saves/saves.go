// Package saves parses the addon's SavedVariables Lua file (SynthiqBotsUI.lua)
// and extracts the feedback_queue[] entries.
//
// We embed gopher-lua and just `dostring` the file — the format is a Lua
// script that assigns globals like:
//
//   SynthiqBotsUISave = {
//       ["AutoChatCommands"] = "true",
//       feedback_queue = { { id="...", ts=..., note="...", ... }, ... },
//   }
//
// Custom regex parsing of this would be fragile (escaped strings inside
// chat history, nested tables, comments) so we let Lua do the work. The
// runtime cost is ~5 ms for a 200 KB file — acceptable for a 5 s poll loop.
package saves

import (
	"fmt"
	"os"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Entry mirrors what the addon writes to SynthiqBotsUISave.feedback_queue[].
// We carry the raw JSON-able forms of chatHistory and ctx because the
// daemon doesn't need to interpret them — it just forwards them to ops-api.
type Entry struct {
	ID             string                   `json:"id"`
	AddonTs        int64                    `json:"addonTs"`
	Note           string                   `json:"note"`
	ScreenshotHint string                   `json:"screenshotHint,omitempty"`
	ChatHistory    []map[string]any `json:"chatHistory,omitempty"`
	Ctx            map[string]any   `json:"ctx,omitempty"`

	// Convenience flat fields used by the upload payload. Pulled from
	// Ctx during decode so the server-side row gets indexable values
	// without re-parsing the JSON blob.
	CharName  string `json:"charName,omitempty"`
	CharRealm string `json:"charRealm,omitempty"`
	CharClass string `json:"charClass,omitempty"`
	CharLvl   int    `json:"charLvl,omitempty"`
	Zone      string `json:"zone,omitempty"`
}

// AddonTimestamp returns the entry's capture time as a Go time.Time, used
// to match Screenshots/ files within the configured window.
func (e *Entry) AddonTimestamp() time.Time {
	return time.Unix(e.AddonTs, 0)
}

// Parse loads the SavedVariables file and returns all feedback_queue[]
// entries. Returns nil and no error if the file doesn't exist (pre-PR-1
// state, or the user hasn't captured anything yet).
func Parse(path string) ([]Entry, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}

	L := lua.NewState()
	defer L.Close()

	if err := L.DoFile(path); err != nil {
		return nil, fmt.Errorf("dofile: %w", err)
	}

	save := L.GetGlobal("SynthiqBotsUISave")
	tbl, ok := save.(*lua.LTable)
	if !ok {
		// Empty/missing global — addon never ran or the user reset.
		return nil, nil
	}

	queue := tbl.RawGetString("feedback_queue")
	queueTbl, ok := queue.(*lua.LTable)
	if !ok {
		return nil, nil
	}

	var out []Entry
	queueTbl.ForEach(func(_ lua.LValue, v lua.LValue) {
		entryTbl, ok := v.(*lua.LTable)
		if !ok {
			return
		}
		e := decodeEntry(entryTbl)
		if e.ID == "" {
			return // skip malformed rows
		}
		out = append(out, e)
	})
	return out, nil
}

func decodeEntry(t *lua.LTable) Entry {
	e := Entry{
		ID:             luaString(t, "id"),
		AddonTs:        luaInt64(t, "ts"),
		Note:           luaString(t, "note"),
		ScreenshotHint: luaString(t, "screenshot_hint"),
	}

	if hist, ok := t.RawGetString("chat_history").(*lua.LTable); ok {
		e.ChatHistory = decodeArray(hist)
	}
	if ctx, ok := t.RawGetString("ctx").(*lua.LTable); ok {
		e.Ctx = decodeMap(ctx)
		// Lift commonly indexed fields so the server can populate
		// dedicated columns without re-parsing the JSON blob.
		e.CharName, e.CharRealm = splitCharRealm(stringFromMap(e.Ctx, "char"))
		e.CharClass = stringFromMap(e.Ctx, "char_class")
		e.CharLvl = intFromMap(e.Ctx, "char_lvl")
		e.Zone = stringFromMap(e.Ctx, "zone")
	}
	return e
}

func luaString(t *lua.LTable, key string) string {
	if s, ok := t.RawGetString(key).(lua.LString); ok {
		return string(s)
	}
	return ""
}

func luaInt64(t *lua.LTable, key string) int64 {
	v := t.RawGetString(key)
	switch n := v.(type) {
	case lua.LNumber:
		return int64(n)
	case lua.LString:
		// time() in some WoW patches returns a string-like; tolerate it.
		var x int64
		fmt.Sscanf(string(n), "%d", &x)
		return x
	}
	return 0
}

// decodeArray walks an array-style Lua table (1-indexed sequence). Mixed
// keys are dropped from the output.
func decodeArray(t *lua.LTable) []map[string]any {
	var out []map[string]any
	for i := 1; ; i++ {
		v := t.RawGetInt(i)
		if v == lua.LNil {
			break
		}
		if sub, ok := v.(*lua.LTable); ok {
			out = append(out, decodeMap(sub))
		}
	}
	return out
}

// decodeMap recursively converts a Lua table to a Go map. Keys are
// stringified; values are bools, numbers, strings, or nested maps/lists.
func decodeMap(t *lua.LTable) map[string]any {
	out := map[string]any{}
	t.ForEach(func(k, v lua.LValue) {
		key := luaKeyToString(k)
		if key == "" {
			return
		}
		out[key] = decodeValue(v)
	})
	// If the map looks array-shaped (1, 2, 3, ...), demote to a slice
	// for cleaner JSON. The caller decides what to do with it.
	return out
}

func decodeValue(v lua.LValue) any {
	switch x := v.(type) {
	case lua.LBool:
		return bool(x)
	case lua.LNumber:
		// Preserve int-ness when possible for cleaner JSON.
		f := float64(x)
		if f == float64(int64(f)) {
			return int64(f)
		}
		return f
	case lua.LString:
		return string(x)
	case *lua.LTable:
		// Distinguish array vs map by looking at first key.
		if isArray(x) {
			out := []any{}
			for i := 1; ; i++ {
				vv := x.RawGetInt(i)
				if vv == lua.LNil {
					break
				}
				out = append(out, decodeValue(vv))
			}
			return out
		}
		return decodeMap(x)
	}
	return nil
}

func isArray(t *lua.LTable) bool {
	first := t.RawGetInt(1)
	if first == lua.LNil {
		return false
	}
	// If at least key 1 exists and there are no string keys in a small
	// sample, treat as array. Cheap heuristic that's right ~always for
	// addon save data.
	hasStr := false
	t.ForEach(func(k, _ lua.LValue) {
		if _, ok := k.(lua.LString); ok {
			hasStr = true
		}
	})
	return !hasStr
}

func luaKeyToString(k lua.LValue) string {
	switch x := k.(type) {
	case lua.LString:
		return string(x)
	case lua.LNumber:
		return fmt.Sprintf("%v", x)
	}
	return ""
}

func stringFromMap(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func intFromMap(m map[string]any, k string) int {
	if v, ok := m[k]; ok {
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		}
	}
	return 0
}

// splitCharRealm splits "CharName-RealmName" into ("CharName", "RealmName").
// The addon writes char as `UnitName .. "-" .. GetRealmName()`.
func splitCharRealm(s string) (string, string) {
	if s == "" {
		return "", ""
	}
	if i := strings.LastIndex(s, "-"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}
