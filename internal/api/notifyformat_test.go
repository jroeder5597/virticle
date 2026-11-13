package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

var poolAlert = Alert{ID: "pool:1:data", Kind: "pool-usage", Severity: "critical", Title: "Pool data is 97% full",
	Message: "Storage pool data on kvm1 uses 970 GiB of 1000 GiB.", Node: 1, Link: "/storage", Since: 1700000000}

func raiseMsg(base string) notifyMsg {
	return notifyMsg{Event: "raise", Alerts: []Alert{poolAlert}, Time: 1700000100, Base: base, Nodes: map[int64]string{1: "kvm1"}}
}

func sameJSON(t *testing.T, name string, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: not JSON: %v\n%s", name, err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: golden is not JSON: %v", name, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s differs.\n got: %s\nwant: %s", name, got, want)
	}
}

// The raw webhook body is a public interface: earlier versions sent exactly these
// bytes (event, alert with its fields in this order, time). Do not edit the string.
func TestLegacyWebhookBodyIsPinned(t *testing.T) {
	const want = `{"event":"raise","alert":{"id":"pool:1:data","kind":"pool-usage","severity":"critical","title":"Pool data is 97% full","message":"Storage pool data on kvm1 uses 970 GiB of 1000 GiB.","node":1,"link":"/storage","since":1700000000},"time":1700000100}`
	if got := string(renderWebhook(raiseMsg("https://virt.example.org"))); got != want {
		t.Errorf("legacy body changed:\n got: %s\nwant: %s", got, want)
	}
	c := raiseMsg("")
	c.Event = "clear"
	if got := string(renderWebhook(c)); !strings.HasPrefix(got, `{"event":"clear","alert":{"id":"pool:1:data"`) {
		t.Errorf("clear body: %s", got)
	}
}

func TestSignature(t *testing.T) {
	body := renderWebhook(raiseMsg(""))
	h := hmac.New(sha256.New, []byte("s3cret"))
	h.Write(body)
	if got, want := signBody("s3cret", body), "sha256="+hex.EncodeToString(h.Sum(nil)); got != want {
		t.Errorf("%s != %s", got, want)
	}
	// A value computed outside this code (python hmac, key "key", message "abc").
	if got := signBody("key", []byte("abc")); got != "sha256=9c196e32dc0175f86f4b1cb89289d6619de6bee699e4c378e68309ed97a1a6ab" {
		t.Errorf("signature %s", got)
	}
}

// Golden payloads. Slack: https://api.slack.com/reference/block-kit/blocks (header, section,
// context, actions with a button); Mattermost: attachments as documented at
// https://developers.mattermost.com/integrate/webhooks/incoming/; Discord:
// https://discord.com/developers/docs/resources/webhook#execute-webhook.
func TestGoldenPayloads(t *testing.T) {
	m := raiseMsg("https://virt.example.org")
	sameJSON(t, "slack", renderSlack(m), `{"text":"Critical: Pool data is 97% full","blocks":[
	 {"type":"header","text":{"type":"plain_text","text":"Critical"}},
	 {"type":"section","text":{"type":"mrkdwn","text":"*Pool data is 97% full*\nStorage pool data on kvm1 uses 970 GiB of 1000 GiB."}},
	 {"type":"context","elements":[{"type":"mrkdwn","text":"Node: kvm1  |  2023-11-14 22:15:00 UTC"}]},
	 {"type":"actions","elements":[{"type":"button","text":{"type":"plain_text","text":"Open in Virticle"},"url":"https://virt.example.org/storage"}]}]}`)
	sameJSON(t, "mattermost", renderMattermost(m), `{"attachments":[{"color":"#c0392b","fallback":"Critical: Pool data is 97% full",
	 "title":"Critical: Pool data is 97% full","title_link":"https://virt.example.org/storage",
	 "text":"Storage pool data on kvm1 uses 970 GiB of 1000 GiB.","fields":[{"short":true,"title":"Node","value":"kvm1"}],
	 "footer":"Virticle, 2023-11-14 22:15:00 UTC"}]}`)
	sameJSON(t, "discord", renderDiscord(m), `{"content":"Critical: Pool data is 97% full","allowed_mentions":{"parse":[]},"embeds":[{
	 "title":"Pool data is 97% full","description":"Storage pool data on kvm1 uses 970 GiB of 1000 GiB.","url":"https://virt.example.org/storage",
	 "color":12597547,"fields":[{"name":"Severity","value":"Critical","inline":true},{"name":"Node","value":"kvm1","inline":true}],
	 "footer":{"text":"Virticle"},"timestamp":"2023-11-14T22:15:00Z"}]}`)

	// No public URL: no links at all.
	for k, b := range map[string][]byte{"slack": renderSlack(raiseMsg("")), "mattermost": renderMattermost(raiseMsg("")), "discord": renderDiscord(raiseMsg(""))} {
		if strings.Contains(string(b), "/storage") || strings.Contains(string(b), "title_link") || strings.Contains(string(b), `"url"`) {
			t.Errorf("%s has a link without a public URL: %s", k, b)
		}
	}

	// Cleared and test messages open with their own word.
	c := m
	c.Event = "clear"
	if !strings.Contains(string(renderSlack(c)), `"text":"Cleared"`) || !strings.Contains(string(renderDiscord(c)), `"content":"Cleared: `) {
		t.Errorf("cleared message lacks the word Cleared")
	}
	tm := notifyMsg{Event: "test", Alerts: []Alert{testAlert("Ops")}, Time: 1700000100}
	for k, b := range map[string][]byte{"slack": renderSlack(tm), "mattermost": renderMattermost(tm), "discord": renderDiscord(tm), "webhook": renderWebhook(tm)} {
		if !strings.Contains(string(b), "Test") {
			t.Errorf("%s test message is not marked: %s", k, b)
		}
	}
	if !strings.HasPrefix(string(renderWebhook(tm)), `{"event":"test","alert":{"id":"test"`) {
		t.Errorf("webhook test body: %s", renderWebhook(tm))
	}
	// A warning says Warning.
	w := m
	w.Alerts = []Alert{{ID: "mem:1", Kind: "host-memory", Severity: "warning", Title: "kvm1 memory is 91% used", Message: "x", Node: 1}}
	if !strings.Contains(string(renderSlack(w)), `"text":"Warning"`) {
		t.Error("warning word missing")
	}
}

func TestMarkupInNamesIsNeutralised(t *testing.T) {
	m := raiseMsg("")
	m.Alerts = []Alert{{ID: "vm:1:x", Kind: "vm-state", Severity: "critical", Title: "<!channel> & @all has crashed", Message: "VM <!here> on kvm1."}}
	s := string(renderSlack(m))
	if strings.Contains(s, "<!channel>") || strings.Contains(s, "<!here>") || !strings.Contains(s, "&lt;!channel&gt; &amp;") {
		t.Errorf("slack text not escaped: %s", s)
	}
	if strings.Contains(string(renderMattermost(m)), "@all") {
		t.Errorf("mattermost @all is live: %s", renderMattermost(m))
	}
	if !strings.Contains(string(renderDiscord(m)), `"parse":[]`) {
		t.Error("discord must forbid mentions")
	}
}

func TestLengthLimits(t *testing.T) {
	long := strings.Repeat("äx", 5000) // 10000 characters
	a := Alert{ID: "x", Kind: "failed-job", Severity: "warning", Title: long, Message: long, Node: 1, Link: "/tasks"}
	m := notifyMsg{Event: "raise", Alerts: []Alert{a}, Time: 1, Base: "https://v", Nodes: map[int64]string{1: long}}

	var sl struct {
		Text   string `json:"text"`
		Blocks []struct {
			Type string `json:"type"`
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(renderSlack(m), &sl); err != nil {
		t.Fatal(err)
	}
	for _, b := range sl.Blocks {
		switch b.Type {
		case "header":
			if n := utf8.RuneCountInString(b.Text.Text); n > 150 {
				t.Errorf("slack header %d", n)
			}
		case "section":
			if n := utf8.RuneCountInString(b.Text.Text); n > 3000 {
				t.Errorf("slack section %d", n)
			}
		}
	}
	var dc struct {
		Content string `json:"content"`
		Embeds  []struct {
			Title, Description string
			Fields             []struct{ Name, Value string }
			Footer             struct{ Text string }
		} `json:"embeds"`
	}
	check := func(m notifyMsg) {
		t.Helper()
		if err := json.Unmarshal(renderDiscord(m), &dc); err != nil {
			t.Fatal(err)
		}
		e := dc.Embeds[0]
		total := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description) + utf8.RuneCountInString(e.Footer.Text)
		for _, f := range e.Fields {
			if utf8.RuneCountInString(f.Value) > 1024 || utf8.RuneCountInString(f.Name) > 256 {
				t.Errorf("discord field too long")
			}
			total += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
		}
		if utf8.RuneCountInString(dc.Content) > 2000 || utf8.RuneCountInString(e.Title) > 256 || utf8.RuneCountInString(e.Description) > 4096 || total > 6000 {
			t.Errorf("discord limits: content %d title %d description %d total %d", utf8.RuneCountInString(dc.Content), utf8.RuneCountInString(e.Title), utf8.RuneCountInString(e.Description), total)
		}
	}
	check(m)

	// A summary of many alerts still fits, and says how many are left out.
	var many []Alert
	for i := 0; i < 300; i++ {
		many = append(many, Alert{ID: "a", Severity: "warning", Title: "Pool data is 97% full on a node with a longish name"})
	}
	sm := notifyMsg{Event: "raise", Alerts: many, Summary: true, Time: 1}
	check(sm)
	if !strings.Contains(string(renderDiscord(sm)), "and ") || !strings.Contains(string(renderSlack(sm)), " more") {
		t.Error("summary does not say that alerts were left out")
	}
	if !strings.Contains(string(renderSlack(sm)), "300 new alerts: 0 Critical, 300 Warning") {
		t.Errorf("summary head: %s", renderSlack(sm)[:200])
	}
}
