package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/mcp"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/observer"
)

// ObserverDeps wires the observer capture tools (screenshot + clip) to the
// capture queue and the gameplay MCP (used to whisper the in-world observer
// character).
type ObserverDeps struct {
	Queue          *observer.Queue
	MCP            *mcp.Client // gameplay MCP client (calls whisper_player)
	ObserverChar   string      // in-world GM observer character name
	WhisperBotGUID uint64      // bot that sends the trigger whisper (e.g. leader 20007)

	ClipTTL    time.Duration // queue lifetime for a clip capture (recording + encode + upload)
	ClipMaxSec int           // longest clip a caller may request
}

// reqIDHex returns a whitespace-free request id safe to embed in a whisper.
func reqIDHex() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// RegisterObserverScreenshotTool registers `request_observer_screenshot`: it
// asks an in-world observer (a real WoW client the user powered up) to take a
// REAL screenshot framed on a target bot, then waits for the feedback-daemon to
// deliver it. Unlike render_tactical_map (a synthetic schematic), this is actual
// game visuals — but it needs the on-demand observer client running. See
// docs/capture.md.
func RegisterObserverScreenshotTool(reg *Registry, deps ObserverDeps) {
	reg.Register(Tool{
		Name: "request_observer_screenshot",
		Description: "Take a REAL in-game screenshot via an on-demand observer client and deliver it. " +
			"Requires the gaming-PC WoW client to be running with the GM observer character logged in " +
			"(power it up first). Whispers the observer to teleport to `target_bot`, frame the shot, and " +
			"capture; the image is hosted and pushed to Telegram/Slack, and the URL is returned. " +
			"Distinct from render_tactical_map, which is a synthetic schematic needing no client. " +
			"Returns {ok, reqId, artifact_url, pushed} or an error if the observer isn't online / times out.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"target_bot":{"type":"string","description":"Character name of the bot to frame the shot on (the observer .appears to it)"},
			"view":{"type":"integer","description":"Optional saved camera-view slot 1-5 to use (default: observer's current view)"}
		},"required":["target_bot"]}`),
		Annotations: json.RawMessage(`{"readOnlyHint":false,"idempotentHint":false,"destructiveHint":false,"openWorldHint":true}`),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TargetBot string `json:"target_bot"`
				View      int    `json:"view"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if a.TargetBot == "" {
				return map[string]any{"error": "target_bot required"}
			}
			if deps.Queue == nil || deps.ObserverChar == "" {
				return map[string]any{"error": "observer capture not configured (OPS_OBSERVER_CHAR empty)"}
			}
			if deps.MCP == nil {
				return map[string]any{"error": "gameplay MCP client not configured (MCP_URL empty)"}
			}
			if a.View < 0 || a.View > 5 {
				a.View = 0
			}

			reqID := reqIDHex()
			capt := deps.Queue.Add(reqID, a.TargetBot, observer.KindShot, 0, 0)

			// Trigger: whisper the structured command to the observer character.
			// The observer addon parses "[SBSHOT] <reqId> <targetBot> <view>".
			msg := fmt.Sprintf("[SBSHOT] %s %s %d", reqID, a.TargetBot, a.View)
			res, err := deps.MCP.CallTool(ctx, "whisper_player", map[string]any{
				"botGuid": deps.WhisperBotGUID,
				"name":    deps.ObserverChar,
				"message": msg,
			})
			if err != nil {
				deps.Queue.Resolve(reqID, observer.Result{Err: err.Error()})
				return map[string]any{"error": "whisper trigger failed: " + err.Error()}
			}
			// whisper_player returns {"error": ...} when the observer is offline.
			var wr struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(res, &wr) == nil && wr.Error != "" {
				deps.Queue.Resolve(reqID, observer.Result{Err: wr.Error})
				return map[string]any{"error": "observer not reachable: " + wr.Error +
					" (is the gaming-PC client up and the observer logged in?)"}
			}

			// Block until the daemon ships the shot (or the queue TTL elapses).
			result, err := deps.Queue.Wait(ctx, capt)
			if err != nil {
				return map[string]any{
					"error": "timed out waiting for the observer screenshot (" + err.Error() +
						") — is the feedback-daemon running on the gaming PC?",
					"reqId": reqID,
				}
			}
			if result.Err != "" {
				return map[string]any{"error": result.Err, "reqId": reqID}
			}
			return map[string]any{
				"ok":           true,
				"reqId":        reqID,
				"artifact_url": result.URL,
				"pushed":       result.Pushed,
			}
		},
	})
}
