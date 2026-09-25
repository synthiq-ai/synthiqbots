package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/artifact"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/notify"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/tacmap"
)

// TacMapDeps carries the artifact store (where the PNG is hosted) and the
// out-of-band notifier (Telegram/Slack). Both may be disabled — the tool still
// renders and returns a URL when the store is configured.
type TacMapDeps struct {
	Store    *artifact.Store
	Notifier *notify.Notifier
}

// tacMapMaxSize bounds the rendered image edge so a bad arg can't ask for a
// gigapixel canvas.
const tacMapMaxSize = 2048

// RegisterTacticalMapTool registers `render_tactical_map`: it takes a scene
// snapshot (produced by the gameplay-MCP get_scene_snapshot tool), renders a
// top-down "radar" PNG, hosts it, pushes it to the configured chat channels,
// and returns the clickable URL. This is the synthetic, no-client alternative
// to a real screenshot — see docs/capture.md.
func RegisterTacticalMapTool(reg *Registry, deps TacMapDeps) {
	reg.Register(Tool{
		Name: "render_tactical_map",
		Description: "Render a top-down tactical 'radar' PNG of an in-game scene and deliver it. " +
			"Pass the object returned by the gameplay-MCP get_scene_snapshot tool as `scene` " +
			"(anchor bot coords + nearby entities). The map is hosted and a clickable URL is " +
			"returned; if Telegram/Slack are configured it is also pushed there (the only way the " +
			"human actually sees the image — MCP tool results don't render images inline). " +
			"This is a synthetic schematic built from server-side coordinates (dots on a plane, no " +
			"terrain art), NOT a real screenshot — bots are server-side and have no camera. " +
			"Returns {ok, artifact_url, pushed:[...], entity_count}.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"scene":{"type":"object","description":"The get_scene_snapshot result: {anchor:{name,x,y,z,o,map_id,zone_id,zone_name}, radius, entities:[{kind,name,x,y,z,hostile,hp_pct,level,is_bot}]}"},
			"size":{"type":"integer","description":"Square image edge in px (default 768, min 256, max 2048)"},
			"caption":{"type":"string","description":"Optional human caption for the chat push"}
		},"required":["scene"]}`),
		// Side-effecting (writes a file, pushes to chat) but not destructive to
		// game/infra state, so it is NOT gated by AdminAllowActions.
		Annotations: json.RawMessage(`{"readOnlyHint":false,"idempotentHint":false,"destructiveHint":false,"openWorldHint":true}`),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Scene   tacmap.Scene `json:"scene"`
				Size    int          `json:"size"`
				Caption string       `json:"caption"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if deps.Store == nil || !deps.Store.Enabled() {
				return map[string]any{"error": "artifact store not configured (OPS_ARTIFACT_DIR empty)"}
			}
			size := a.Size
			if size == 0 {
				size = 768
			}
			if size > tacMapMaxSize {
				size = tacMapMaxSize
			}

			png, err := tacmap.Render(a.Scene, size)
			if err != nil {
				return map[string]any{"error": "render: " + err.Error()}
			}
			rel, url, err := deps.Store.Save("tacmap", "png", png)
			if err != nil {
				return map[string]any{"error": "store: " + err.Error()}
			}

			caption := a.Caption
			if caption == "" {
				caption = defaultTacMapCaption(a.Scene)
			}

			res := map[string]any{
				"ok":           true,
				"artifact_url": url,
				"artifact_rel": rel,
				"entity_count": len(a.Scene.Entities),
				"bytes":        len(png),
				"pushed":       []string{},
			}
			if deps.Notifier != nil && deps.Notifier.Enabled() {
				pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				pr := deps.Notifier.Push(pctx, notify.Artifact{
					Kind:     notify.KindImage,
					Bytes:    png,
					Filename: tacMapFilename(a.Scene),
					MIME:     "image/png",
					Caption:  caption,
					URL:      url,
				})
				res["pushed"] = pr.Pushed
				if len(pr.Errors) > 0 {
					res["push_errors"] = pr.Errors
				}
			}
			return res
		},
	})
}

func defaultTacMapCaption(s tacmap.Scene) string {
	zone := s.Anchor.ZoneName
	if zone == "" {
		zone = fmt.Sprintf("map %d", s.Anchor.MapID)
	}
	return fmt.Sprintf("%s — %s + %d nearby", zone, firstNonEmpty(s.Anchor.Name, "anchor"), len(s.Entities))
}

func tacMapFilename(s tacmap.Scene) string {
	zone := s.Anchor.ZoneName
	if zone == "" {
		zone = "map"
	}
	return fmt.Sprintf("tacmap_%s.png", slugify(zone))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// slugify keeps [a-z0-9-] for a filesystem/URL-safe label.
func slugify(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			out = append(out, r+32)
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			out = append(out, r)
		case r == ' ' || r == '-' || r == '_':
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "scene"
	}
	return string(out)
}
