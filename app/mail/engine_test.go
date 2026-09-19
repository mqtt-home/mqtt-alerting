package mail

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mqtt-home/mqtt-mail/config"
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
	return Event{Kind: kind, At: at, Alert: Alert{Rule: "bridge-offline", Topic: topic, Value: "offline", Since: at}}
}

func TestMailerBatchesAndRespectsBudget(t *testing.T) {
	var subjects []string
	m := NewMailer(mailCfg())
	m.send = func(_ config.SMTPConfig, subject, _ string) error {
		subjects = append(subjects, subject)
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
	if subjects[1] != "[home] RESOLVED bridge-offline: a/bridge/state" {
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
	m.send = func(config.SMTPConfig, string, string) error {
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

func TestComposeBody(t *testing.T) {
	ev := event(EventResolved, "haus/shelly/bridge/state", t0)
	ev.Duration = "12m 3s"
	ev.Alert.Description = "a bridge reports offline"
	_, body := compose("", []Event{ev}, t0)
	for _, want := range []string{"RESOLVED", "haus/shelly/bridge/state", "a bridge reports offline", "firing: 12m 3s"} {
		if !strings.Contains(body, want) {
			t.Errorf("body misses %q:\n%s", want, body)
		}
	}
}
