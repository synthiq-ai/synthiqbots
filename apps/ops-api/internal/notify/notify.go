// Package notify pushes captured artifacts (tactical maps, observer
// screenshots, video clips) to out-of-band chat channels — Telegram and Slack.
//
// Why out-of-band: an MCP tool result cannot deliver an image to the human
// (the Anthropic clients don't render tool-result image blocks inline — see
// docs/capture.md), so a capture's PNG/MP4 reaches the user by being pushed to
// a chat channel, while the MCP tool result carries only a clickable URL.
//
// Each channel is independent and best-effort: a Notifier with an empty token
// for a channel simply skips it. Push never fails the caller — it returns the
// list of channels that accepted the artifact plus any per-channel errors, so a
// capture still succeeds (URL returned) even when every push fails.
package notify

import (
	"context"
	"net/http"
	"time"
)

// Kind distinguishes a still image from a video clip so each backend can pick
// the right API method (Telegram sendPhoto vs sendVideo) and MIME handling.
type Kind int

const (
	KindImage Kind = iota
	KindVideo
)

// Artifact is one thing to push: raw bytes plus enough metadata for the
// backends. Filename is what the user sees in the chat client.
type Artifact struct {
	Kind     Kind
	Bytes    []byte
	Filename string // e.g. "tacmap_durotar_1718000000.png"
	MIME     string // e.g. "image/png", "video/mp4"
	Caption  string // short human caption, e.g. "Durotar — fleet + 4 hostiles"
	URL      string // the hosted artifact URL (appended to the caption)
}

// Result reports which channels accepted the artifact and which failed. The
// caller logs Errors but does not treat them as fatal.
type Result struct {
	Pushed []string          // channel names that accepted, e.g. ["telegram","slack"]
	Errors map[string]string // channel name -> error string, for the ones that failed
}

// Notifier fans an Artifact out to every configured channel. Construct via New;
// a channel with an empty credential is omitted from the fan-out entirely.
type Notifier struct {
	telegram *telegramClient
	slack    *slackClient
	http     *http.Client
}

// Options carries the per-channel credentials. Empty fields disable that
// channel. Mirrors the config keys in internal/config.
type Options struct {
	TelegramBotToken string
	TelegramChatID   string
	SlackBotToken    string
	SlackChannel     string

	// HTTPClient is optional; tests inject one pointed at a stub server. When
	// nil a 30s-timeout client is used (uploads are small still images / short
	// clips, but video can be a few MB on a slow link).
	HTTPClient *http.Client
}

// New builds a Notifier from Options. It never returns an error — missing
// credentials just mean fewer active channels (an all-empty Options yields a
// Notifier whose Push reports Pushed:[] every time).
func New(o Options) *Notifier {
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	n := &Notifier{http: hc}
	if o.TelegramBotToken != "" && o.TelegramChatID != "" {
		n.telegram = &telegramClient{
			token:  o.TelegramBotToken,
			chatID: o.TelegramChatID,
			http:   hc,
		}
	}
	if o.SlackBotToken != "" && o.SlackChannel != "" {
		n.slack = &slackClient{
			token:   o.SlackBotToken,
			channel: o.SlackChannel,
			http:    hc,
		}
	}
	return n
}

// Enabled reports whether at least one channel is configured. Callers use it to
// annotate the MCP tool result ("pushed":[] vs an actual list) and to skip the
// fan-out entirely when nothing is wired.
func (n *Notifier) Enabled() bool { return n.telegram != nil || n.slack != nil }

// Push fans the artifact out to every configured channel concurrently and
// collects the outcome. It is best-effort: a per-channel failure is recorded in
// Result.Errors but does not abort the others or return a top-level error.
func (n *Notifier) Push(ctx context.Context, a Artifact) Result {
	res := Result{Errors: map[string]string{}}
	type outcome struct {
		name string
		err  error
	}
	var pending int
	ch := make(chan outcome, 2)
	if n.telegram != nil {
		pending++
		go func() { ch <- outcome{"telegram", n.telegram.push(ctx, a)} }()
	}
	if n.slack != nil {
		pending++
		go func() { ch <- outcome{"slack", n.slack.push(ctx, a)} }()
	}
	for i := 0; i < pending; i++ {
		o := <-ch
		if o.err != nil {
			res.Errors[o.name] = o.err.Error()
			continue
		}
		res.Pushed = append(res.Pushed, o.name)
	}
	return res
}
