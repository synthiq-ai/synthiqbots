package tacmap

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func sampleScene() Scene {
	return Scene{
		Anchor: Anchor{
			Name: "Claude", X: 100, Y: 200, Z: 10, O: 0,
			MapID: 1, ZoneID: 14, ZoneName: "Durotar",
		},
		Radius: 60,
		Entities: []Entity{
			{Kind: "creature", Name: "Clattering Scorpid", X: 130, Y: 210, Hostile: true, HPPct: 100, Level: 7},
			{Kind: "player", Name: "Clawd", X: 105, Y: 205, IsBot: true, Level: 8},
			{Kind: "questgiver", Name: "Gornek", X: 90, Y: 180, Level: 12},
			{Kind: "gameobject", Name: "Mailbox", X: 110, Y: 220},
			// Deliberately out of radius — must be clamped, not crash.
			{Kind: "creature", Name: "FarBoar", X: 100, Y: 9999, Hostile: true, Level: 4},
		},
	}
}

func TestRenderProducesValidPNG(t *testing.T) {
	data, err := Render(sampleScene(), 768)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.Width != 768 || cfg.Height != 768 {
		t.Errorf("dims = %dx%d, want 768x768", cfg.Width, cfg.Height)
	}

	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := distinctColors(img); got < 4 {
		t.Errorf("only %d distinct colors — render looks blank", got)
	}
}

func TestRenderDefaultsGuardDivByZero(t *testing.T) {
	// Zero radius + no entities must not panic and must still render.
	s := Scene{Anchor: Anchor{Name: "X", MapID: 0}}
	data, err := Render(s, 100) // sub-min size also exercised
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	cfg, _ := png.DecodeConfig(bytes.NewReader(data))
	if cfg.Width < 256 {
		t.Errorf("width = %d, want clamped to >=256", cfg.Width)
	}
}

// distinctColors counts unique RGBA values (capped) — a cheap "is it blank?"
// signal without pixel-exact assertions.
func distinctColors(img image.Image) int {
	seen := map[uint32]struct{}{}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y += 3 {
		for x := b.Min.X; x < b.Max.X; x += 3 {
			r, g, bl, a := img.At(x, y).RGBA()
			key := (r>>8)<<24 | (g>>8)<<16 | (bl>>8)<<8 | (a >> 8)
			seen[key] = struct{}{}
			if len(seen) >= 16 {
				return len(seen)
			}
		}
	}
	return len(seen)
}
