package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/observer"
)

// Clip length bounds. The floor keeps ffmpeg's startup cost from dominating the
// recording; the ceiling is a safety net when OPS_OBSERVER_CLIP_MAX_SEC is unset.
const (
	clipMinSec         = 3
	clipDefaultSec     = 15
	clipFallbackMaxSec = 30
)

// RegisterObserverClipTools registers the Phase 3 pair:
//
//	request_observer_clip — fire-and-forget: start a recording, return a reqId
//	get_observer_clip     — poll that reqId for the hosted URL once it lands
//
// Unlike request_observer_screenshot, this does NOT block. A 20s clip needs
// roughly 40-70s end to end (teleport + record + encode + upload) while the MCP
// transport gives up around 60s — a blocking tool would report a timeout for a
// clip that actually succeeded. So the tool returns immediately, the clip is
// pushed to Slack when ready, and the URL is retrievable afterwards.
func RegisterObserverClipTools(reg *Registry, deps ObserverDeps) {
	maxSec := deps.ClipMaxSec
	if maxSec <= 0 {
		maxSec = clipFallbackMaxSec
	}

	reg.Register(Tool{
		Name: "request_observer_clip",
		Description: "Record a REAL in-game video clip via an on-demand observer client and deliver it. " +
			"Requires the gaming-PC WoW client to be running (windowed, not exclusive-fullscreen) with the " +
			"GM observer character logged in, the feedback-daemon running, and ffmpeg installed. Whispers the " +
			"observer to teleport to `target_bot`; once in position the daemon records `seconds` of video, " +
			"which is hosted and pushed to Telegram/Slack. " +
			"ASYNC: returns {ok, reqId, status:\"recording\"} immediately — call get_observer_clip{req_id} " +
			"afterwards for the artifact_url. For a still image use request_observer_screenshot instead.",
		InputSchema: json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{
			"target_bot":{"type":"string","description":"Character name of the bot to film (the observer .appears to it)"},
			"seconds":{"type":"integer","description":"Clip length in seconds (default %d, min %d, max %d)"},
			"view":{"type":"integer","description":"Optional saved camera-view slot 1-5 to use (default: observer's current view)"}
		},"required":["target_bot"]}`, clipDefaultSec, clipMinSec, maxSec)),
		Annotations: json.RawMessage(`{"readOnlyHint":false,"idempotentHint":false,"destructiveHint":false,"openWorldHint":true}`),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				TargetBot string `json:"target_bot"`
				Seconds   int    `json:"seconds"`
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
			if a.Seconds == 0 {
				a.Seconds = clipDefaultSec
			}
			if a.Seconds < clipMinSec {
				a.Seconds = clipMinSec
			}
			if a.Seconds > maxSec {
				a.Seconds = maxSec
			}

			reqID := reqIDHex()
			deps.Queue.Add(reqID, a.TargetBot, observer.KindClip, a.Seconds, deps.ClipTTL)

			// Trigger: whisper the structured command to the observer character.
			// The observer addon parses "[SBCLIP] <reqId> <targetBot> <view> <seconds>".
			msg := fmt.Sprintf("[SBCLIP] %s %s %d %d", reqID, a.TargetBot, a.View, a.Seconds)
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

			// Deliberately do NOT Wait: recording outlives the MCP transport.
			return map[string]any{
				"ok":      true,
				"reqId":   reqID,
				"status":  "recording",
				"seconds": a.Seconds,
				"note": fmt.Sprintf("recording %ds of %s; the clip will be pushed to Slack when ready. "+
					"Poll get_observer_clip{req_id:%q} for the URL.", a.Seconds, a.TargetBot, reqID),
			}
		},
	})

	reg.Register(Tool{
		Name: "get_observer_clip",
		Description: "Fetch the result of a request_observer_clip by its reqId. " +
			"Returns {status:\"ready\", artifact_url, pushed} once the clip is hosted, " +
			"{status:\"recording\"} while it is still being captured, or {status:\"expired\"} if the reqId is " +
			"unknown, already reaped, or the observer never delivered.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"req_id":{"type":"string","description":"The reqId returned by request_observer_clip"}
		},"required":["req_id"]}`),
		Annotations: json.RawMessage(`{"readOnlyHint":true,"idempotentHint":true,"destructiveHint":false,"openWorldHint":false}`),
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				ReqID string `json:"req_id"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if a.ReqID == "" {
				return map[string]any{"error": "req_id required"}
			}
			if deps.Queue == nil {
				return map[string]any{"error": "observer capture not configured (OPS_OBSERVER_CHAR empty)"}
			}

			// Resolved captures (success or failure) are retained for a while.
			if r, ok := deps.Queue.Lookup(a.ReqID); ok {
				if r.Err != "" {
					return map[string]any{"ok": false, "status": "failed", "reqId": a.ReqID, "error": r.Err}
				}
				return map[string]any{
					"ok":           true,
					"status":       "ready",
					"reqId":        a.ReqID,
					"artifact_url": r.URL,
					"pushed":       r.Pushed,
				}
			}
			for _, p := range deps.Queue.Pending() {
				if p.ReqID == a.ReqID {
					return map[string]any{"ok": true, "status": "recording", "reqId": a.ReqID, "seconds": p.Seconds}
				}
			}
			return map[string]any{
				"ok":     false,
				"status": "expired",
				"reqId":  a.ReqID,
				"error": "unknown or expired reqId — the observer never delivered " +
					"(is the feedback-daemon running, and is ffmpeg installed?)",
			}
		},
	})
}
