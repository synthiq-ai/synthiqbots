package dispatch

// Prompt assembly. Two outputs:
//
//   - System prompt: locks the agent into "module-improvement" framing,
//     reminds it which MCP tools it has, scopes the deliverable.
//   - User content: text block describing the captured event + a
//     base64-encoded image_url block carrying the screenshot. Built as
//     OpenAI-compatible content blocks so Synthiq's chat-completions
//     endpoint accepts it without a custom format.

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// SystemPrompt is the locked system message the dispatcher sends. We
// keep it short and concrete — the agent has its own MCP tool catalog
// to reference, no need to re-document those here.
const SystemPrompt = `You are a code-improvement agent for the mod-ollama-chat AzerothCore module — a C++ server module that lets WoW player-bots speak through an LLM. The user has flagged a moment in-game where something went wrong, with a screenshot, the chat that led up to it, and game context.

You have MCP tools (admin-mcp on https://ops.wow.example.com/mcp) for live config tweaks (file_set_key, db_query, db_exec, container_restart) and direct shell/git/gh access for repo edits and PRs against synthiq-ai/synthiqbots (main).

For each feedback you receive, decide ONE of these actions and explain why in 2-4 sentences:

1. **Live config tweak** — invoke file_set_key on a mod_ollama_chat.conf key (allowlisted patterns only) and call container_restart on ac-worldserver. Use this for tuning knobs (cooldowns, model overrides, channel gates, tactical heartbeat).

2. **Code-fix PR** — branch from main, apply the smallest patch that addresses the root cause, push, open a PR with a clear title + body. Use this for actual bugs (wrong condition, missing case, broken sql).

3. **Need more info** — answer with a clarifying question + the data you'd need (e.g. "show me the gateway audit row from this minute"). Use this only if you genuinely cannot tell what action would help.

Do NOT take any action unless you're confident it addresses the root cause. Wrong action is worse than no action — false positives churn the codebase.

Respond with a JSON object:

  {
    "action": "config_tweak" | "code_pr" | "need_info" | "no_action",
    "summary": "one sentence summary of decision",
    "reasoning": "2-4 sentences why",
    "config_changes": [{"file": "...", "key": "...", "old": "...", "new": "..."}],
    "pr_url": "https://github.com/.../pull/..." (only if action == code_pr),
    "follow_up_question": "..." (only if action == need_info)
  }

Empty arrays / strings are fine for fields that don't apply.`

// BuildUserContent returns the OpenAI-style content array for the user
// message: a text block + (when imageBytes is non-empty) an image_url
// data-URL block. Falls back to text-only when no screenshot is present.
//
// This matches the shape both OpenAI and Anthropic accept for vision
// inputs in chat-completions mode. Synthiq presents an OpenAI-compatible
// surface, so this should pass through.
func BuildUserContent(p *PendingRow, imageBytes []byte) []map[string]any {
	textBlock := map[string]any{
		"type": "text",
		"text": buildTextBody(p),
	}
	if len(imageBytes) == 0 {
		return []map[string]any{textBlock}
	}
	dataURL := "data:" + p.ImageMime + ";base64," + base64.StdEncoding.EncodeToString(imageBytes)
	return []map[string]any{
		textBlock,
		{
			"type":      "image_url",
			"image_url": map[string]any{"url": dataURL},
		},
	}
}

// buildTextBody assembles the readable description the agent sees alongside
// the screenshot. We keep it compact — the screenshot carries most of the
// signal, the text just supplies context the screenshot can't show.
func buildTextBody(p *PendingRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## User feedback (id=%s)\n\n", p.AddonID)
	if p.Note != "" {
		fmt.Fprintf(&b, "**Note from user:** %s\n\n", p.Note)
	} else {
		b.WriteString("**Note from user:** (none — captured by button click)\n\n")
	}
	fmt.Fprintf(&b, "**Character:** %s-%s — %s level %d\n",
		nz(p.CharName, "?"), nz(p.CharRealm, "?"),
		nz(p.CharClass, "?"), p.CharLvl)
	fmt.Fprintf(&b, "**Zone:** %s\n", nz(p.Zone, "?"))
	fmt.Fprintf(&b, "**Captured at:** unix=%d (UTC)\n\n", p.AddonTs)

	if strings.TrimSpace(p.CtxJSON) != "" {
		fmt.Fprintf(&b, "**Full context (JSON):**\n```json\n%s\n```\n\n", p.CtxJSON)
	}
	if strings.TrimSpace(p.ChatHistory) != "" {
		fmt.Fprintf(&b, "**Recent chat (JSON, oldest→newest):**\n```json\n%s\n```\n\n", p.ChatHistory)
	}

	b.WriteString("Look at the screenshot, the chat that led up to it, and the user's note. " +
		"Decide on ONE action per the system prompt. Return only the JSON object.")
	return b.String()
}

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
