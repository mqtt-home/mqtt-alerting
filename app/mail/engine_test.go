package mail

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mqtt-home/mqtt-alerting/config"
)

var t0 = time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)

func rules(t *testing.T, js string) []config.RuleConfig {
	t.Helper()
	var r []config.RuleConfig
	if err := json.Unmarshal([]byte(js), &r); err != nil {
		t.Fatalf("bad test rules: %v", err)
	}
	return r
}

func newEngine(t *testing.T, js string) (*Engine, *[]Event) {
	t.Helper()
	e, err := NewEngine(rules(t, js), t0)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	var got []Event
	e.OnEvent(func(ev Event) { got = append(got, ev) })
	return e, &got
}

func kinds(evs []Event) string {
	var k []string
	for _, ev := range evs {
		k = append(k, ev.Kind+":"+ev.Alert.Topic)
	}
	return strings.Join(k, " ")
}

const offlineRule = `[{
	"name": "bridge-offline", "type": "state",
	"topics": ["+/bridge/state", "+/+/bridge/state"],
	"exclude": ["test/#"],
	"condition": {"equals": "offline"},
	"for": "5m", "repeat": "1h"
}]`

func TestStateFiresOnlyAfterFor(t *testing.T) {
	e, got := newEngine(t, offlineRule)

	e.HandleMessage("haus/shelly/bridge/state", []byte("offline"), t0)
	e.Tick(t0.Add(4 * time.Minute))
	if len(*got) != 0 {
		t.Fatalf("fired early: %s", kinds(*got))
	}

	e.Tick(t0.Add(5 * time.Minute))
	if kinds(*got) != "firing:haus/shelly/bridge/state" {
		t.Fatalf("got %q", kinds(*got))
	}

	// Firing once is enough: further ticks stay quiet until the reminder.
	e.Tick(t0.Add(30 * time.Minute))
	if len(*got) != 1 {
		t.Fatalf("repeated too early: %s", kinds(*got))
	}
	e.Tick(t0.Add(65 * time.Minute))
	if (*got)[1].Kind != EventReminder {
		t.Fatalf("no reminder: %s", kinds(*got))
	}

	e.HandleMessage("haus/shelly/bridge/state", []byte("online"), t0.Add(70*time.Minute))
	last := (*got)[len(*got)-1]
	if last.Kind != EventResolved || last.Duration != "1h 5m" {
		t.Fatalf("got %+v", last)
	}
}

// A restart publishes offline then online within seconds. That must not mail.
func TestStateBlipDoesNotFire(t *testing.T) {
	e, got := newEngine(t, offlineRule)

	e.HandleMessage("rules/bridge/state", []byte("offline"), t0)
	e.HandleMessage("rules/bridge/state", []byte("online"), t0.Add(20*time.Second))
	e.Tick(t0.Add(10 * time.Minute))

	if len(*got) != 0 {
		t.Fatalf("blip fired: %s", kinds(*got))
	}
}

func TestExcludeAndPerTopicInstances(t *testing.T) {
	e, got := newEngine(t, offlineRule)

	e.HandleMessage("test/unifi-live/bridge/state", []byte("offline"), t0)
	e.HandleMessage("sonos/bridge/state", []byte("offline"), t0)
	e.HandleMessage("hue/bridge/state", []byte("online"), t0)
	e.Tick(t0.Add(6 * time.Minute))

	if kinds(*got) != "firing:sonos/bridge/state" {
		t.Fatalf("got %q", kinds(*got))
	}
}

func TestJSONFieldCondition(t *testing.T) {
	e, got := newEngine(t, `[{
		"name": "z2m", "type": "state", "topics": ["zigbee2mqtt/bridge/state"],
		"condition": {"field": "state", "equals": "offline"}, "for": "1m"
	}, {
		"name": "battery", "type": "state", "topics": ["zigbee2mqtt/+"],
		"condition": {"field": "battery", "lt": 10}
	}]`)

	e.HandleMessage("zigbee2mqtt/bridge/state", []byte(`{"state":"offline"}`), t0)
	e.HandleMessage("zigbee2mqtt/door", []byte(`{"battery":100,"contact":true}`), t0)
	e.HandleMessage("zigbee2mqtt/window", []byte(`{"battery":7}`), t0)
	e.Tick(t0.Add(2 * time.Minute))

	if kinds(*got) != "firing:zigbee2mqtt/window firing:zigbee2mqtt/bridge/state" {
		t.Fatalf("got %q", kinds(*got))
	}
	if (*got)[0].Alert.Value != "7" {
		t.Fatalf("value = %q", (*got)[0].Alert.Value)
	}
}

// The doorbell on 2026-09-19: healthy-looking device, reconnecting forever.
func TestCountRule(t *testing.T) {
	e, got := newEngine(t, `[{
		"name": "reconnect-loop", "type": "count",
		"topics": ["haus/unifi-access-doorbell/bridge/logs"],
		"condition": {"regex": "Reconnect attempt|WATCHDOG"},
		"count": 5, "within": "10m"
	}]`)
	topic := "haus/unifi-access-doorbell/bridge/logs"

	for i := 0; i < 4; i++ {
		e.HandleMessage(topic, []byte("WebSocket: Reconnect attempt 1"), t0.Add(time.Duration(i)*time.Minute))
		e.HandleMessage(topic, []byte("UniFi: Login successful"), t0.Add(time.Duration(i)*time.Minute))
	}
	if len(*got) != 0 {
		t.Fatalf("fired below threshold: %s", kinds(*got))
	}

	e.HandleMessage(topic, []byte("WATCHDOG: WebSocket disconnected for 909s"), t0.Add(5*time.Minute))
	if kinds(*got) != "firing:"+topic {
		t.Fatalf("got %q", kinds(*got))
	}

	// Slower but still happening: keep firing instead of flapping.
	e.Tick(t0.Add(12 * time.Minute))
	if len(*got) != 1 {
		t.Fatalf("flapped: %s", kinds(*got))
	}

	e.Tick(t0.Add(16 * time.Minute))
	if (*got)[len(*got)-1].Kind != EventResolved {
		t.Fatalf("not resolved after a quiet window: %s", kinds(*got))
	}
}

func TestSilenceRule(t *testing.T) {
	e, got := newEngine(t, `[{
		"name": "strom-silent", "type": "silence", "topics": ["haus/strom"], "for": "15m"
	}, {
		"name": "weather-silent", "type": "silence", "topics": ["garden/weather/+"], "for": "15m"
	}]`)

	// The concrete topic never said anything — that is exactly the case to catch.
	e.HandleMessage("garden/weather/wind", []byte("1"), t0.Add(10*time.Minute))
	e.Tick(t0.Add(16 * time.Minute))
	if kinds(*got) != "firing:haus/strom" {
		t.Fatalf("got %q", kinds(*got))
	}

	e.HandleMessage("haus/strom", []byte("{}"), t0.Add(20*time.Minute))
	e.Tick(t0.Add(26 * time.Minute))
	if kinds(*got) != "firing:haus/strom resolved:haus/strom firing:garden/weather/wind" {
		t.Fatalf("got %q", kinds(*got))
	}
}

func TestRecoveryCanBeDisabled(t *testing.T) {
	e, got := newEngine(t, `[{
		"name": "r", "type": "state", "topics": ["a"], "condition": {"equals": "x"}, "recovery": false
	}]`)
	e.HandleMessage("a", []byte("x"), t0)
	e.HandleMessage("a", []byte("y"), t0.Add(time.Minute))
	if kinds(*got) != "firing:a" {
		t.Fatalf("got %q", kinds(*got))
	}
	if len(e.GetStatus().Alerts) != 0 {
		t.Fatal("alert still listed after it resolved")
	}
}

func TestInvalidRulesAreRejected(t *testing.T) {
	for name, js := range map[string]string{
		"unknown type":   `[{"name":"a","type":"nope","topics":["a"]}]`,
		"no condition":   `[{"name":"a","type":"state","topics":["a"]}]`,
		"empty cond":     `[{"name":"a","type":"state","topics":["a"],"condition":{}}]`,
		"bad regex":      `[{"name":"a","type":"state","topics":["a"],"condition":{"regex":"("}}]`,
		"count no win":   `[{"name":"a","type":"count","topics":["a"],"condition":{"regex":"x"},"count":3}]`,
		"silence no for": `[{"name":"a","type":"silence","topics":["a"]}]`,
		"no topics":      `[{"name":"a","type":"silence","for":"1m"}]`,
		"duplicate":      `[{"name":"a","type":"silence","topics":["a"],"for":"1m"},{"name":"a","type":"silence","topics":["b"],"for":"1m"}]`,
	} {
		if _, err := NewEngine(rules(t, js), t0); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTopicMatching(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"+/bridge/state", "sonos/bridge/state", true},
		{"+/bridge/state", "haus/shelly/bridge/state", false},
		{"+/+/bridge/state", "haus/shelly/bridge/state", true},
		{"test/#", "test/a/b/c", true},
		{"test/#", "test", true},
		{"test/#", "testing/a", false},
		{"a/b", "a/b/c", false},
		{"#", "anything/at/all", true},
	}
	for _, c := range cases {
		if got := topicMatches(c.filter, c.topic); got != c.want {
			t.Errorf("topicMatches(%q, %q) = %v", c.filter, c.topic, got)
		}
	}
}

func TestSubscriptionsDropCoveredFilters(t *testing.T) {
	e, _ := newEngine(t, `[
		{"name":"a","type":"silence","for":"1m","topics":["haus/#","haus/strom","+/bridge/state"]},
		{"name":"b","type":"silence","for":"1m","topics":["sonos/bridge/state","haus/#"]}
	]`)
	got := strings.Join(e.Subscriptions(), " ")
	if got != "+/bridge/state haus/#" {
		t.Fatalf("got %q", got)
	}
}

func mailCfg() config.MailConfig {
	return config.MailConfig{
		SMTP:            config.SMTPConfig{Enabled: true, Host: "h", From: "f", To: "t"},
		SubjectPrefix:   "[home]",
		BatchSeconds:    30,
		MaxMailsPerHour: 2,
	}
}

func event(kind, topic string, at time.Time) Event {
	return Event{Kind: kind, At: at, Alert: Alert{Rule: "bridge-offline", Title: topic + " is offline", Topic: topic, Value: "offline", Since: at}}
}

func TestMailerBatchesAndRespectsBudget(t *testing.T) {
	var subjects []string
	m := NewMailer(mailCfg())
	m.send = func(_ config.SMTPConfig, mail Mail) error {
		subjects = append(subjects, mail.Subject)
		return nil
	}

	m.Enqueue(event(EventFiring, "a/bridge/state", t0))
	m.Enqueue(event(EventFiring, "b/bridge/state", t0.Add(5*time.Second)))
	m.Flush(t0.Add(10 * time.Second))
	if len(subjects) != 0 {
		t.Fatal("sent inside the batch window")
	}
	m.Flush(t0.Add(31 * time.Second))
	if len(subjects) != 1 || subjects[0] != "[home] 2 alerts (2 alert)" {
		t.Fatalf("got %q", subjects)
	}

	m.Enqueue(event(EventResolved, "a/bridge/state", t0.Add(2*time.Minute)))
	m.Flush(t0.Add(3 * time.Minute))
	if subjects[1] != "[home] RESOLVED: a/bridge/state is offline" {
		t.Fatalf("got %q", subjects[1])
	}

	// Budget of 2/h is used up: held back, not dropped.
	m.Enqueue(event(EventFiring, "c/bridge/state", t0.Add(4*time.Minute)))
	m.Flush(t0.Add(10 * time.Minute))
	if len(subjects) != 2 || m.Stats().Queued != 1 {
		t.Fatalf("budget ignored: %q queued=%d", subjects, m.Stats().Queued)
	}
	m.Flush(t0.Add(62 * time.Minute))
	if len(subjects) != 3 {
		t.Fatalf("never delivered after the budget freed up: %q", subjects)
	}
}

func TestMailerRetriesAfterFailure(t *testing.T) {
	fail := true
	sent := 0
	m := NewMailer(mailCfg())
	m.send = func(config.SMTPConfig, Mail) error {
		if fail {
			return errors.New("535 authentication failed")
		}
		sent++
		return nil
	}

	m.Enqueue(event(EventFiring, "a", t0))
	m.Flush(t0.Add(time.Minute))
	if st := m.Stats(); st.Queued != 1 || !strings.Contains(st.LastError, "535") {
		t.Fatalf("stats after failure: %+v", st)
	}

	fail = false
	m.Flush(t0.Add(2 * time.Minute))
	if st := m.Stats(); sent != 1 || st.Queued != 0 || st.LastError != "" {
		t.Fatalf("stats after retry: sent=%d %+v", sent, st)
	}
}

func TestTitlesUseWhatTheWildcardsMatched(t *testing.T) {
	e, got := newEngine(t, `[{
		"name": "bridge-offline", "type": "state", "title": "{device} is offline",
		"topics": ["+/+/bridge/state"], "condition": {"equals": "offline"}
	}, {
		"name": "battery", "type": "state", "title": "{device}: battery at {value} %",
		"topics": ["zigbee2mqtt/+"], "condition": {"field": "battery", "lt": 15}
	}, {
		"name": "shelly", "type": "state", "title": "Shelly {device} is unreachable",
		"topics": ["shelly/+/+/+/online"], "condition": {"equals": "false"}
	}]`)

	e.HandleMessage("haus/shelly/bridge/state", []byte("offline"), t0)
	e.HandleMessage("zigbee2mqtt/eg_cont_haustuere", []byte(`{"battery":7}`), t0)
	e.HandleMessage("shelly/og/leni/east/online", []byte("false"), t0)

	want := []string{"haus/shelly is offline", "eg_cont_haustuere: battery at 7 %", "Shelly og/leni/east is unreachable"}
	for i, w := range want {
		if (*got)[i].Alert.Title != w {
			t.Errorf("title %d = %q, want %q", i, (*got)[i].Alert.Title, w)
		}
	}
}

// 51 topics under wolf-cwl/# each have their own rhythm. What matters is
// whether the service says anything at all.
func TestGroupSilenceWatchesTheFilterAsAWhole(t *testing.T) {
	e, got := newEngine(t, `[{
		"name": "data-silent", "type": "silence", "group": true, "title": "{device} stopped publishing",
		"topics": ["wolf-cwl/#", "garden/weather/#"], "for": "30m"
	}]`)

	e.HandleMessage("wolf-cwl/temperature/supply", []byte("21"), t0.Add(time.Minute))
	for i := 1; i < 7; i++ {
		e.HandleMessage("wolf-cwl/airflow/current_volume", []byte("140"), t0.Add(time.Duration(i*10)*time.Minute))
	}
	e.Tick(t0.Add(61 * time.Minute))

	// wolf-cwl kept talking (one quiet topic does not matter), the weather
	// station never said a word.
	if kinds(*got) != "firing:garden/weather/#" {
		t.Fatalf("got %q", kinds(*got))
	}
	if (*got)[0].Alert.Title != "garden/weather stopped publishing" {
		t.Fatalf("title = %q", (*got)[0].Alert.Title)
	}
	if n := e.GetStatus().Rules[0].Watching; n != 2 {
		t.Fatalf("watching %d instances, want one per filter", n)
	}

	e.HandleMessage("garden/weather/wind", []byte("3"), t0.Add(70*time.Minute))
	if (*got)[1].Kind != EventResolved {
		t.Fatalf("got %q", kinds(*got))
	}
}

func TestGroupOnlyForSilence(t *testing.T) {
	_, err := NewEngine(rules(t, `[{"name":"a","type":"state","group":true,"topics":["a/#"],"condition":{"equals":"x"}}]`), t0)
	if err == nil {
		t.Fatal("accepted group on a state rule")
	}
}

// Overlapping subscriptions make the broker deliver one message twice.
func TestCountIgnoresDuplicateDeliveries(t *testing.T) {
	e, got := newEngine(t, `[{
		"name": "loop", "type": "count", "topics": ["a/logs"],
		"condition": {"regex": "Reconnect"}, "count": 3, "within": "10m"
	}]`)

	e.HandleMessage("a/logs", []byte("Reconnect attempt 1"), t0)
	e.HandleMessage("a/logs", []byte("Reconnect attempt 1"), t0.Add(5*time.Millisecond))
	e.HandleMessage("a/logs", []byte("Reconnect attempt 2"), t0.Add(time.Second))
	e.HandleMessage("a/logs", []byte("Reconnect attempt 2"), t0.Add(time.Second+5*time.Millisecond))
	if len(*got) != 0 {
		t.Fatalf("two real messages counted as four: %s", kinds(*got))
	}

	// The same line again a minute later is a new occurrence, not a duplicate.
	e.HandleMessage("a/logs", []byte("Reconnect attempt 2"), t0.Add(time.Minute))
	if kinds(*got) != "firing:a/logs" {
		t.Fatalf("got %q", kinds(*got))
	}
}

func sampleStatus() *Status {
	return &Status{
		Rules: []RuleInfo{
			{Name: "bridge-offline", Description: "a bridge reports offline", Watching: 19, Firing: 2, OK: 17},
			{Name: "battery-low", Summary: "battery < 15 for 1h", Watching: 24, OK: 24},
			{Name: "data-silent", Description: "a service stopped publishing", Watching: 12, Pending: 1, OK: 11},
			{Name: "node-disk-full", Type: "promql", Summary: "disk > 85", Blind: true},
		},
		Alerts: []Alert{
			{Rule: "bridge-offline", Title: "haus/shelly is offline", Topic: "haus/shelly/bridge/state", State: StateFiring, Since: t0},
			{Rule: "bridge-offline", Title: "sonos is offline", Topic: "sonos/bridge/state", State: StateFiring, Since: t0},
		},
	}
}

func TestComposeSaysWhatIsWrongAndWhatWorks(t *testing.T) {
	fired := event(EventFiring, "haus/shelly/bridge/state", t0)
	fired.Alert.Description = "a bridge reports offline"
	resolved := event(EventResolved, "rules/bridge/state", t0)
	resolved.Duration = "12m 3s"

	mail := compose([]Event{fired, resolved}, renderOptions{Prefix: "[home]", UIURL: "https://mail.example", Now: t0, Status: sampleStatus()})

	if mail.Subject != "[home] 2 alerts (1 alert, 1 resolved)" {
		t.Errorf("subject = %q", mail.Subject)
	}
	for _, body := range []string{mail.Text, mail.HTML} {
		for _, want := range []string{
			"1 problem needs attention",           // headline
			"haus/shelly/bridge/state is offline", // what went wrong
			"12m 3s",                              // how long the other one was down
			"sonos is offline",                    // open problem this mail is not about
			"battery-low",                         // overview: a rule that is fine
			"https://mail.example",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("body misses %q", want)
			}
		}
	}
	// The alert this mail is about must not show up again under "other open problems".
	if strings.Count(mail.Text, "haus/shelly is offline") != 0 {
		t.Error("alert repeated under other open problems")
	}
	for _, want := range []string{"✓ OK", "2 firing", "1 pending", "no watch query"} {
		if !strings.Contains(mail.HTML, want) {
			t.Errorf("overview pill %q missing", want)
		}
	}
	// Each rule says how much of what it watches is fine, in both parts.
	for _, body := range []string{mail.Text, mail.HTML} {
		for _, want := range []string{"17 of 19 fine", "all 24 fine", "11 of 12 fine", "coverage unknown"} {
			if !strings.Contains(body, want) {
				t.Errorf("coverage %q missing", want)
			}
		}
	}
}

func TestHeadlineWhenEverythingRecovered(t *testing.T) {
	resolved := event(EventResolved, "rules/bridge/state", t0)
	clear := &Status{Rules: []RuleInfo{{Name: "bridge-offline", Watching: 19}}}

	if head, kind := headline([]Event{resolved}, renderOptions{Status: clear}); head != "1 thing works again — all clear" || kind != EventResolved {
		t.Errorf("got %q / %s", head, kind)
	}
	// Good news must not hide that something else is still broken.
	if head, kind := headline([]Event{resolved}, renderOptions{Status: sampleStatus()}); head != "1 thing works again, 2 problems still open" || kind != EventReminder {
		t.Errorf("got %q / %s", head, kind)
	}
}

func TestHTMLEscapesPayloads(t *testing.T) {
	ev := event(EventFiring, "a/logs", t0)
	ev.Alert.Value = `<script>alert("x")</script>`
	mail := compose([]Event{ev}, renderOptions{Now: t0})
	if strings.Contains(mail.HTML, "<script>") {
		t.Fatal("payload reached the HTML unescaped")
	}
}

func TestMimeBody(t *testing.T) {
	headers, body := mimeBody(Mail{Text: "plain ünïcode", HTML: "<b>html</b>"}, "BOUNDARY")
	if len(headers) != 1 || !strings.Contains(headers[0], `multipart/alternative; boundary="BOUNDARY"`) {
		t.Fatalf("headers = %q", headers)
	}
	if strings.Count(body, "--BOUNDARY\r\n") != 2 || !strings.HasSuffix(body, "--BOUNDARY--\r\n") {
		t.Fatalf("bad multipart structure:\n%s", body)
	}
	if strings.Index(body, "text/plain") > strings.Index(body, "text/html") {
		t.Fatal("html must be the last alternative")
	}
	for _, line := range strings.Split(body, "\r\n") {
		if len(line) > 76 {
			t.Fatalf("line of %d chars", len(line))
		}
	}

	headers, _ = mimeBody(Mail{Text: "only text"}, "B")
	if len(headers) != 2 || !strings.HasPrefix(headers[0], "Content-Type: text/plain") {
		t.Fatalf("plain headers = %q", headers)
	}
}

func TestTestMailShowsEveryStyle(t *testing.T) {
	var sent Mail
	m := NewMailer(mailCfg())
	m.send = func(_ config.SMTPConfig, mail Mail) error { sent = mail; return nil }
	m.SetSnapshot(func() Status { return *sampleStatus() })

	if err := m.SendTest(t0); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"made up", "What went wrong", "Still not fixed", "Working again", "Overview"} {
		if !strings.Contains(sent.HTML, want) {
			t.Errorf("test mail misses %q", want)
		}
	}
	if sent.Subject != "[home] Test mail" {
		t.Errorf("subject = %q", sent.Subject)
	}
}

func TestResolvedTitleAndCurrentValue(t *testing.T) {
	e, got := newEngine(t, `[{
		"name": "bridge-offline", "type": "state", "topics": ["+/bridge/state"],
		"title": "{device} is offline", "resolved_title": "{device} is back online",
		"condition": {"equals": "offline"}
	}, {
		"name": "quiet", "type": "silence", "topics": ["a"], "for": "1m"
	}]`)

	e.HandleMessage("sonos/bridge/state", []byte("offline"), t0)
	e.HandleMessage("sonos/bridge/state", []byte("online"), t0.Add(time.Minute))
	resolved := (*got)[1]
	if resolved.Alert.Title != "sonos is back online" || resolved.Alert.Value != "online" {
		t.Fatalf("got %q / %q", resolved.Alert.Title, resolved.Alert.Value)
	}

	e.Tick(t0.Add(2 * time.Minute))
	e.HandleMessage("a", []byte("x"), t0.Add(3*time.Minute))
	last := (*got)[len(*got)-1]
	if last.Alert.Title != "a: back to normal" || last.Alert.Value != "" {
		t.Fatalf("got %q / %q", last.Alert.Title, last.Alert.Value)
	}
}

// A device that was removed: its retained message gets deleted (empty payload).
func TestDeletedRetainedMessageForgetsTheDevice(t *testing.T) {
	e, got := newEngine(t, `[{
		"name": "shelly", "type": "state", "topics": ["shelly/+/+/+/online"],
		"title": "Shelly {device} is unreachable", "condition": {"equals": "false"}, "for": "15m"
	}]`)

	e.HandleMessage("shelly/eg/wohnzimmer/ost/online", []byte("false"), t0)
	e.HandleMessage("shelly/og/leni/east/online", []byte("true"), t0)
	e.Tick(t0.Add(16 * time.Minute))
	if kinds(*got) != "firing:shelly/eg/wohnzimmer/ost/online" {
		t.Fatalf("got %q", kinds(*got))
	}

	e.HandleMessage("shelly/eg/wohnzimmer/ost/online", nil, t0.Add(20*time.Minute))

	if (*got)[len(*got)-1].Kind != EventResolved {
		t.Fatalf("still firing after the topic was deleted: %s", kinds(*got))
	}
	st := e.GetStatus()
	if len(st.Alerts) != 0 || st.Rules[0].Watching != 1 {
		t.Fatalf("deleted device is still tracked: alerts=%d watching=%d", len(st.Alerts), st.Rules[0].Watching)
	}
	// ...and it must not come back by itself.
	e.Tick(t0.Add(2 * time.Hour))
	if n := len(*got); n != 2 {
		t.Fatalf("got %d events: %s", n, kinds(*got))
	}
}

// v0.2.0 stopped processing messages and ticking after four hours while
// /livez kept answering "healthy", so nothing restarted it.
func TestLivenessNoticesAStuckEngine(t *testing.T) {
	e, _ := newEngine(t, offlineRule)

	if !e.healthyAt(t0.Add(30 * time.Second)) {
		t.Fatal("unhealthy right after start")
	}

	e.Tick(t0.Add(5 * time.Minute))
	e.HandleMessage("hue/bridge/state", []byte("online"), t0.Add(5*time.Minute))
	if !e.healthyAt(t0.Add(5*time.Minute + 10*time.Second)) {
		t.Fatal("unhealthy while ticking and receiving")
	}

	// still ticking, but deaf: the subscription died
	e.Tick(t0.Add(16 * time.Minute))
	if e.healthyAt(t0.Add(16*time.Minute + time.Second)) {
		t.Fatal("healthy although no message arrived for 11 minutes")
	}

	// receiving, but the tick loop is wedged
	e.HandleMessage("hue/bridge/state", []byte("online"), t0.Add(20*time.Minute))
	if e.healthyAt(t0.Add(20*time.Minute + time.Second)) {
		t.Fatal("healthy although the engine stopped ticking")
	}
}
