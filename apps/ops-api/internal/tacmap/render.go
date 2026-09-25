// Package tacmap renders a synthetic top-down "radar" of an in-game scene from
// server-side coordinates. AzerothCore bots have no client/camera, so this is
// the autonomous, no-hardware alternative to a real screenshot: dots on a flat
// plane (no terrain art), enough to answer "where is everyone / what's around
// the bot." The Scene contract is produced by the gameplay-MCP get_scene_snapshot
// tool (src/mod-ollama-chat_tools.cpp) and consumed here. See docs/capture.md.
package tacmap

import (
	"bytes"
	"fmt"
	"image/color"
	"math"

	"github.com/fogleman/gg"
)

// Anchor is the bot the map is centred on (its own coordinates).
type Anchor struct {
	Name     string  `json:"name"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Z        float64 `json:"z"`
	O        float64 `json:"o"` // orientation, radians (0 = +X / "north")
	MapID    int     `json:"map_id"`
	ZoneID   int     `json:"zone_id"`
	ZoneName string  `json:"zone_name"`
}

// Entity is one nearby thing to plot. Kind ∈ {player, creature, gameobject,
// questgiver}; Hostile drives colour for creatures.
type Entity struct {
	Kind    string  `json:"kind"`
	Name    string  `json:"name"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Z       float64 `json:"z"`
	Hostile bool    `json:"hostile"`
	HPPct   int     `json:"hp_pct"`
	Level   int     `json:"level"`
	IsBot   bool    `json:"is_bot"`
}

// Scene is the full render input: the anchor, the radius the snapshot covered
// (yards), and every entity inside it.
type Scene struct {
	Anchor   Anchor   `json:"anchor"`
	Radius   float64  `json:"radius"`
	Entities []Entity `json:"entities"`
}

// palette — dark slate background with high-contrast entity dots.
var (
	colBG       = color.RGBA{0x14, 0x17, 0x1c, 0xff}
	colRing     = color.RGBA{0x33, 0x3a, 0x44, 0xff}
	colGrid     = color.RGBA{0x22, 0x27, 0x2e, 0xff}
	colAnchor   = color.RGBA{0x4f, 0xc3, 0xf7, 0xff} // cyan — the bot we centre on
	colHostile  = color.RGBA{0xef, 0x53, 0x50, 0xff} // red
	colFriendly = color.RGBA{0x66, 0xbb, 0x6a, 0xff} // green — players/bots
	colObject   = color.RGBA{0x9e, 0x9e, 0x9e, 0xff} // grey — gameobjects
	colQuest    = color.RGBA{0xff, 0xca, 0x28, 0xff} // gold — questgivers
	colText     = color.RGBA{0xe0, 0xe0, 0xe0, 0xff}
	colTextDim  = color.RGBA{0x8a, 0x93, 0x9e, 0xff}
)

// Render draws the scene to a PNG and returns the encoded bytes. size is the
// square image edge in pixels (e.g. 768). A zero/te tiny radius falls back to a
// sane default so the projection never divides by zero.
func Render(scene Scene, size int) ([]byte, error) {
	if size < 256 {
		size = 256
	}
	radius := scene.Radius
	if radius < 1 {
		radius = 60 // default snapshot radius
	}

	dc := gg.NewContext(size, size)
	dc.SetColor(colBG)
	dc.Clear()

	center := float64(size) / 2
	margin := 28.0
	usable := center - margin
	scale := usable / radius // pixels per yard

	// Range rings at full + half radius, plus N/S/E/W crosshair.
	dc.SetColor(colGrid)
	dc.SetLineWidth(1)
	dc.DrawLine(center, margin, center, float64(size)-margin)
	dc.DrawLine(margin, center, float64(size)-margin, center)
	dc.Stroke()
	dc.SetColor(colRing)
	for _, frac := range []float64{1.0, 0.5} {
		dc.DrawCircle(center, center, usable*frac)
		dc.Stroke()
	}

	// project maps world (x,y) to screen pixels. WoW: +X is north, +Y is west.
	// Screen has +x right, +y down, so north is up and west is left:
	//   sx = center - (y - anchor.Y)*scale   (west/+Y → left)
	//   sy = center - (x - anchor.X)*scale   (north/+X → up)
	project := func(x, y float64) (float64, float64) {
		sx := center - (y-scene.Anchor.Y)*scale
		sy := center - (x-scene.Anchor.X)*scale
		// Clamp to the usable disc so a slightly-out-of-range entity still shows.
		dx, dy := sx-center, sy-center
		if d := math.Hypot(dx, dy); d > usable {
			sx = center + dx/d*usable
			sy = center + dy/d*usable
		}
		return sx, sy
	}

	// Entities first so the anchor marker sits on top.
	for _, e := range scene.Entities {
		sx, sy := project(e.X, e.Y)
		dc.SetColor(entityColor(e))
		dc.DrawCircle(sx, sy, 5)
		dc.Fill()
		if label := entityLabel(e); label != "" {
			dc.SetColor(colTextDim)
			dc.DrawStringAnchored(label, sx, sy-9, 0.5, 0.5)
		}
	}

	// Anchor marker: filled diamond + orientation arrow.
	dc.SetColor(colAnchor)
	drawDiamond(dc, center, center, 6)
	// Orientation: o=0 points +X (north/up). Arrow tip in that direction.
	ax := center - math.Sin(scene.Anchor.O)*16 // +Y component (west) maps to -sx
	ay := center - math.Cos(scene.Anchor.O)*16 // +X component (north) maps to -sy
	dc.SetLineWidth(2)
	dc.DrawLine(center, center, ax, ay)
	dc.Stroke()
	dc.SetColor(colText)
	dc.DrawStringAnchored(safeName(scene.Anchor.Name), center, center+14, 0.5, 0.5)

	drawHeader(dc, scene, size)
	drawLegend(dc, size)

	var buf bytes.Buffer
	if err := dc.EncodePNG(&buf); err != nil {
		return nil, fmt.Errorf("encode png: %w", err)
	}
	return buf.Bytes(), nil
}

func entityColor(e Entity) color.RGBA {
	switch e.Kind {
	case "gameobject":
		return colObject
	case "questgiver":
		return colQuest
	case "player":
		return colFriendly
	case "creature":
		if e.Hostile {
			return colHostile
		}
		return colObject
	default:
		if e.Hostile {
			return colHostile
		}
		return colObject
	}
}

func entityLabel(e Entity) string {
	n := safeName(e.Name)
	if n == "" {
		return ""
	}
	if len(n) > 16 {
		n = n[:16]
	}
	if e.Level > 0 && (e.Kind == "creature" || e.Kind == "player") {
		return fmt.Sprintf("%s (%d)", n, e.Level)
	}
	return n
}

func drawHeader(dc *gg.Context, scene Scene, size int) {
	dc.SetColor(colText)
	zone := safeName(scene.Anchor.ZoneName)
	if zone == "" {
		zone = fmt.Sprintf("map %d", scene.Anchor.MapID)
	}
	dc.DrawString(fmt.Sprintf("%s  ·  map %d  ·  r=%.0fyd  ·  %d nearby",
		zone, scene.Anchor.MapID, scene.Radius, len(scene.Entities)), 8, 16)
	_ = size
}

func drawLegend(dc *gg.Context, size int) {
	items := []struct {
		c     color.RGBA
		label string
	}{
		{colAnchor, "anchor"},
		{colHostile, "hostile"},
		{colFriendly, "player"},
		{colQuest, "questgiver"},
		{colObject, "object/neutral"},
	}
	y := float64(size) - 10
	x := 8.0
	for _, it := range items {
		dc.SetColor(it.c)
		dc.DrawCircle(x+4, y-4, 4)
		dc.Fill()
		dc.SetColor(colTextDim)
		dc.DrawString(it.label, x+12, y)
		x += float64(len(it.label))*7 + 26
	}
}

func drawDiamond(dc *gg.Context, cx, cy, r float64) {
	dc.MoveTo(cx, cy-r)
	dc.LineTo(cx+r, cy)
	dc.LineTo(cx, cy+r)
	dc.LineTo(cx-r, cy)
	dc.ClosePath()
	dc.Fill()
}

// safeName strips control characters that could corrupt the rendered string.
func safeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 0x20 && r != 0x7f {
			out = append(out, r)
		}
	}
	return string(out)
}
