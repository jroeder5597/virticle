package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// What is sent to a destination, written by hand for each service's incoming
// webhook. The documents these follow:
//
//	Slack        https://api.slack.com/messaging/webhooks and the Block Kit reference
//	             https://api.slack.com/reference/block-kit/blocks (header text 150 characters,
//	             section text 3000, at most 50 blocks) and
//	             https://api.slack.com/reference/surfaces/formatting#escaping (& < > are escaped)
//	Mattermost   https://developers.mattermost.com/integrate/webhooks/incoming/ (text in Markdown,
//	             "attachments" as in Slack's legacy attachments: fallback, color, title,
//	             title_link, text, fields[title,value,short])
//	Discord      https://discord.com/developers/docs/resources/webhook#execute-webhook and
//	             https://discord.com/developers/docs/resources/message#embed-object-embed-limits
//	             (content 2000; embed title 256, description 4096, 25 fields, field name 256,
//	             value 1024, footer 2048, 6000 characters in all)
//
// The raw webhook keeps the body earlier versions sent (webhookMsg).

// notifyMsg is one message to one destination.
type notifyMsg struct {
	Event   string  // raise | clear | test
	Alerts  []Alert // one alert, or several in a summary
	Summary bool
	Time    int64
	// What the text needs from outside the alert: the controller's public address
	// (empty when none is set, then no links are made) and node names by id.
	Base  string
	Nodes map[int64]string
}

// webhookMsg is the body of the raw webhook. Its shape is a public interface
// (documented in docs/reference.md): do not change fields or their order.
type webhookMsg struct {
	Event string `json:"event"` // raise | clear (test for the test message)
	Alert Alert  `json:"alert"`
	Time  int64  `json:"time"`
}

func renderWebhook(m notifyMsg) []byte {
	b, _ := json.Marshal(webhookMsg{Event: m.Event, Alert: m.Alerts[0], Time: m.Time})
	return b
}

// signBody is the value of the X-Virticle-Signature header.
func signBody(secret string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	return "sha256=" + hex.EncodeToString(h.Sum(nil))
}

// clip shortens s to at most n characters (not bytes).
func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-3]) + "..."
}

func sevWord(sev string) string {
	if sev == "critical" {
		return "Critical"
	}
	return "Warning"
}

// word is the severity word that opens a message; colour is never the only signal.
func (m notifyMsg) word() string {
	switch {
	case m.Event == "test":
		return "Test"
	case m.Event == "clear":
		return "Cleared"
	}
	return sevWord(m.Alerts[0].Severity)
}

// head is the first line of a message.
func (m notifyMsg) head() string {
	if !m.Summary {
		return m.word()
	}
	n := len(m.Alerts)
	if m.Event == "clear" {
		return fmt.Sprintf("%d alerts cleared", n)
	}
	crit := 0
	for _, a := range m.Alerts {
		if a.Severity == "critical" {
			crit++
		}
	}
	return fmt.Sprintf("%d new alerts: %d Critical, %d Warning", n, crit, n-crit)
}

func (m notifyMsg) title() string {
	if m.Summary {
		return m.head()
	}
	return m.Alerts[0].Title
}

// body is the alert text. A cleared alert gets its title only: its message
// describes the trouble as it was.
func (m notifyMsg) body() string {
	if m.Summary {
		return ""
	}
	a := m.Alerts[0]
	if m.Event == "clear" {
		return "No longer active."
	}
	return a.Message
}

func (m notifyMsg) link(a Alert) string {
	if m.Base == "" || a.Link == "" {
		return ""
	}
	return strings.TrimRight(m.Base, "/") + a.Link
}

func (m notifyMsg) node(a Alert) string { return m.Nodes[a.Node] }

// lines lists the alerts of a summary, one per line, within max characters.
func (m notifyMsg) lines(max int, bullet string, esc func(string) string) string {
	var b strings.Builder
	for i, a := range m.Alerts {
		w := sevWord(a.Severity)
		if m.Event == "clear" {
			w = "Cleared"
		}
		line := bullet + w + ": " + esc(a.Title) + "\n"
		if b.Len()+len(line) > max-60 {
			fmt.Fprintf(&b, "and %d more", len(m.Alerts)-i)
			break
		}
		b.WriteString(line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// jsonOut writes JSON without turning < > & into \u escapes, which keeps the text readable in logs and tests.
func jsonOut(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// --- Slack ---

// slackEsc escapes the three characters Slack treats as markup; it also stops
// "<!channel>" in a VM name from notifying a whole channel.
func slackEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func renderSlack(m notifyMsg) []byte {
	type obj = map[string]any
	text := func(kind, s string) obj { return obj{"type": kind, "text": s} }
	var detail string
	var link string
	if m.Summary {
		detail = m.lines(3000, "• ", slackEsc)
	} else {
		a := m.Alerts[0]
		detail = "*" + slackEsc(shorten(m.title(), 300)) + "*"
		if b := m.body(); b != "" {
			detail += "\n" + slackEsc(shorten(b, 2500))
		}
		link = m.link(a)
	}
	blocks := []obj{
		{"type": "header", "text": text("plain_text", shorten(m.head(), 150))},
		{"type": "section", "text": text("mrkdwn", shorten(detail, 3000))},
	}
	var ctx []string
	if !m.Summary {
		if n := m.node(m.Alerts[0]); n != "" {
			ctx = append(ctx, "Node: "+slackEsc(n))
		}
	}
	ctx = append(ctx, time.Unix(m.Time, 0).UTC().Format("2006-01-02 15:04:05 UTC"))
	blocks = append(blocks, obj{"type": "context", "elements": []obj{text("mrkdwn", strings.Join(ctx, "  |  "))}})
	if link != "" {
		blocks = append(blocks, obj{"type": "actions", "elements": []obj{{
			"type": "button", "text": text("plain_text", "Open in Virticle"), "url": link}}})
	}
	fallback := m.head()
	if !m.Summary {
		fallback += ": " + m.title()
	}
	return jsonOut(obj{"text": slackEsc(shorten(fallback, 3000)), "blocks": blocks})
}

// --- Mattermost ---

// mmEsc breaks "@channel" and "@all" so that a name inside an alert cannot
// notify a whole channel.
func mmEsc(s string) string { return strings.ReplaceAll(s, "@", "@​") }

func mmColor(m notifyMsg) string {
	switch {
	case m.Event == "test":
		return "#5b7fa6"
	case m.Event == "clear":
		return "#2e8b57"
	case m.Alerts[0].Severity == "critical":
		return "#c0392b"
	}
	return "#d68910"
}

func renderMattermost(m notifyMsg) []byte {
	type obj = map[string]any
	att := obj{"color": mmColor(m)}
	title := m.head()
	if !m.Summary {
		title += ": " + m.title()
	}
	att["fallback"] = shorten(mmEsc(title), 1000)
	att["title"] = shorten(mmEsc(title), 1000)
	if m.Summary {
		att["text"] = shorten(m.lines(4000, "- ", mmEsc), 4000)
	} else {
		a := m.Alerts[0]
		att["text"] = shorten(mmEsc(m.body()), 4000)
		if l := m.link(a); l != "" {
			att["title_link"] = l
		}
		if n := m.node(a); n != "" {
			att["fields"] = []obj{{"short": true, "title": "Node", "value": mmEsc(n)}}
		}
	}
	att["footer"] = "Virticle, " + time.Unix(m.Time, 0).UTC().Format("2006-01-02 15:04:05 UTC")
	return jsonOut(obj{"attachments": []obj{att}})
}

// --- Discord ---

func discordColor(m notifyMsg) int {
	switch {
	case m.Event == "test":
		return 0x5b7fa6
	case m.Event == "clear":
		return 0x2e8b57
	case m.Alerts[0].Severity == "critical":
		return 0xc0392b
	}
	return 0xd68910
}

func renderDiscord(m notifyMsg) []byte {
	type obj = map[string]any
	emb := obj{"color": discordColor(m), "timestamp": time.Unix(m.Time, 0).UTC().Format(time.RFC3339), "footer": obj{"text": "Virticle"}}
	// content is the plain-text line (it shows in notifications and in clients that
	// do not draw embeds); the embed carries the detail.
	content := m.head()
	emb["title"] = shorten(m.head(), 256)
	if m.Summary {
		emb["description"] = shorten(m.lines(4000, "- ", func(s string) string { return s }), 4000)
	} else {
		a := m.Alerts[0]
		content += ": " + m.title()
		emb["title"] = shorten(m.title(), 256)
		emb["description"] = shorten(m.body(), 4000)
		if l := m.link(a); l != "" {
			emb["url"] = l
		}
		fields := []obj{{"name": "Severity", "value": m.word(), "inline": true}}
		if n := m.node(a); n != "" {
			fields = append(fields, obj{"name": "Node", "value": shorten(n, 1000), "inline": true})
		}
		emb["fields"] = fields
	}
	// allowed_mentions with an empty parse list: nothing in the text can ping anyone.
	return jsonOut(obj{"content": shorten(content, 2000), "embeds": []obj{emb}, "allowed_mentions": obj{"parse": []string{}}})
}

// render picks the body for a destination type.
func render(kind string, m notifyMsg) []byte {
	switch kind {
	case "slack":
		return renderSlack(m)
	case "mattermost":
		return renderMattermost(m)
	case "discord":
		return renderDiscord(m)
	}
	return renderWebhook(m)
}

// testAlert is the alert of a test message.
func testAlert(name string) Alert {
	return Alert{ID: "test", Kind: "test", Severity: "warning", Title: "Test message from Virticle",
		Message: "This is a test. If you can read it, alerts sent to " + name + " will arrive here. Nothing is wrong."}
}
