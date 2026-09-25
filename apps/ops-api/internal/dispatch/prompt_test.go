package dispatch

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestBuildUserContentNoImage(t *testing.T) {
	row := &PendingRow{
		AddonID: "abc-1", AddonTs: 1700000000,
		CharName: "the operator", CharRealm: "SynthiqEU",
		CharClass: "PRIEST", CharLvl: 47, Zone: "Stranglethorn Vale",
		Note: "bot pulled twice",
	}
	got := BuildUserContent(row, nil)
	if len(got) != 1 || got[0]["type"] != "text" {
		t.Fatalf("expected single text block, got %+v", got)
	}
	text := got[0]["text"].(string)
	for _, want := range []string{"abc-1", "the operator-SynthiqEU", "PRIEST level 47", "Stranglethorn Vale", "bot pulled twice"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q", want)
		}
	}
}

func TestBuildUserContentWithImage(t *testing.T) {
	row := &PendingRow{AddonID: "abc-2", ImageMime: "image/x-tga"}
	imgBytes := []byte{0x00, 0x01, 0x02, 0x03}
	got := BuildUserContent(row, imgBytes)
	if len(got) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(got))
	}
	if got[1]["type"] != "image_url" {
		t.Errorf("second block type: %v", got[1])
	}
	imgBlock := got[1]["image_url"].(map[string]any)
	url := imgBlock["url"].(string)
	if !strings.HasPrefix(url, "data:image/x-tga;base64,") {
		t.Errorf("data URL prefix: %q", url)
	}
	// Verify base64 round-trips back to the original bytes.
	enc := strings.TrimPrefix(url, "data:image/x-tga;base64,")
	decoded, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(imgBytes) {
		t.Errorf("decoded mismatch")
	}
}

func TestBuildUserContentEmptyOptionalFields(t *testing.T) {
	row := &PendingRow{AddonID: "abc-3", AddonTs: 1, Note: ""}
	got := BuildUserContent(row, nil)
	text := got[0]["text"].(string)
	if !strings.Contains(text, "(none — captured by button click)") {
		t.Errorf("expected 'no note' marker, got %q", text)
	}
	if !strings.Contains(text, "?-?") {
		t.Errorf("expected fallback chars for empty char/realm")
	}
}

func TestBuildUserContentEmbedsCtxAndChat(t *testing.T) {
	row := &PendingRow{
		AddonID:     "abc-4",
		ChatHistory: `[{"text":"go"}]`,
		CtxJSON:     `{"zone":"STV"}`,
	}
	got := BuildUserContent(row, nil)
	text := got[0]["text"].(string)
	if !strings.Contains(text, "```json") {
		t.Errorf("expected fenced JSON blocks in text")
	}
	if !strings.Contains(text, `"zone":"STV"`) {
		t.Errorf("ctx_json not embedded: %q", text)
	}
	if !strings.Contains(text, `"text":"go"`) {
		t.Errorf("chat_history not embedded: %q", text)
	}
}
