package replay

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mqtt-home/mqtt-alerting/config"
	"github.com/mqtt-home/mqtt-alerting/mail"
)

var t0 = time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)

// fakeLogger answers /api/events like mqtt-logger does, from a fixed list,
// honouring topic filter, inclusive from/to and limit (with "truncated").
type fakeLogger struct {
	events   []Message
	requests atomic.Int32
	failures atomic.Int32 // answer this many requests with 502 first
}

func (f *fakeLogger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if f.failures.Load() > 0 {
		f.failures.Add(-1)
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("Bad Gateway"))
		return
	}
	q := r.URL.Query()
	from, _ := time.Parse(time.RFC3339, q.Get("from"))
	to, _ := time.Parse(time.RFC3339, q.Get("to"))
	var limit int
	fmt.Sscan(q.Get("limit"), &limit)

	type ev struct {
		Timestamp string          `json:"timestamp"`
		Topic     string          `json:"topic"`
		Message   json.RawMessage `json:"message"`
	}
	var out []ev
	truncated := false
	for _, m := range f.events {
		if m.Time.Before(from) || m.Time.After(to) || !matches(q.Get("topic"), m.Topic) {
			continue
		}
		if len(out) == limit {
			truncated = true
			break
		}
		msg := json.RawMessage(m.Payload)
		if !json.Valid(m.Payload) {
			msg, _ = json.Marshal(string(m.Payload))
		}
		out = append(out, ev{m.Time.Format(time.RFC3339), m.Topic, msg})
	}
	json.NewEncoder(w).Encode(map[string]any{"events": out, "count": len(out), "truncated": truncated})
}

func msg(sec int, topic, payload string) Message {
	return Message{Time: t0.Add(time.Duration(sec) * time.Second), Topic: topic, Payload: []byte(payload)}
}

func TestFetchRestoresPayloads(t *testing.T) {
	fake := &fakeLogger{events: []Message{
		msg(0, "a/bridge/state", "offline"),
		msg(1, "a/status", `{"state":"idle","battery":86}`),
		msg(2, "a/count", "42"),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	h := &History{BaseURL: srv.URL, Client: srv.Client()}
	got, err := h.Fetch(context.Background(), []string{"a/#"}, t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"offline", `{"state":"idle","battery":86}`, "42"}
	for i, w := range want {
		if string(got[i].Payload) != w {
			t.Errorf("payload %d = %q, want %q", i, got[i].Payload, w)
		}
	}
}

// A busy topic does not fit one page: the window is split until it does,
// without losing or duplicating the message on a split boundary.
func TestFetchSplitsTruncatedWindows(t *testing.T) {
	var events []Message
	for i := 0; i < 50; i++ {
		events = append(events, msg(i*60, "busy/x", fmt.Sprint(i)))
	}
	fake := &fakeLogger{events: events}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	h := &History{BaseURL: srv.URL, Client: srv.Client(), PageSize: 7}
	got, err := h.Fetch(context.Background(), []string{"busy/#"}, t0, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Fatalf("got %d messages, want 50", len(got))
	}
	for i, m := range got {
		if string(m.Payload) != fmt.Sprint(i) {
			t.Fatalf("message %d is %q: order or completeness broken", i, m.Payload)
		}
	}
	if fake.requests.Load() < 8 {
		t.Fatalf("only %d requests - the window was not split", fake.requests.Load())
	}
}

// Overlapping filters must not deliver a message twice; the live broker does not.
func TestFetchOverlappingFiltersNoDuplicates(t *testing.T) {
	fake := &fakeLogger{events: []Message{msg(0, "home/robo/availability", "offline"), msg(1, "home/robo/status", "{}")}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	h := &History{BaseURL: srv.URL, Client: srv.Client()}
	got, err := h.Fetch(context.Background(), []string{"+/+/availability", "home/robo/#"}, t0, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(got), got)
	}
}

// One 502 (the logger restarting) is waited out; the second ends the replay
// instead of hammering a struggling service.
func TestFetchRetriesOnceOnServerError(t *testing.T) {
	fake := &fakeLogger{events: []Message{msg(0, "a/b", "x")}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	fake.failures.Store(1)
	h := &History{BaseURL: srv.URL, Client: srv.Client(), RetryWait: time.Millisecond}
	if got, err := h.Fetch(context.Background(), []string{"a/#"}, t0, t0.Add(time.Hour)); err != nil || len(got) != 1 {
		t.Fatalf("one 502 should be retried: got %d, err %v", len(got), err)
	}

	fake.failures.Store(2)
	if _, err := h.Fetch(context.Background(), []string{"a/#"}, t0, t0.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("two 502 in a row must fail, got %v", err)
	}
}

func TestSplitSkipsPromQLAndForcesRecovery(t *testing.T) {
	off := false
	rules := []config.RuleConfig{
		{Name: "a", Type: config.RuleSilence, Topics: []string{"a"}, For: config.Duration(time.Minute), Recovery: &off},
		{Name: "disk", Type: config.RulePromQL, Query: "x > 1"},
	}
	got, skipped, unknown := Split(rules, nil)
	if len(got) != 1 || len(skipped) != 1 || skipped[0] != "disk" || len(unknown) != 0 {
		t.Fatalf("got %v skipped %v unknown %v", got, skipped, unknown)
	}
	if got[0].Recovery == nil || !*got[0].Recovery {
		t.Fatal("a replay must report recoveries even for rules that do not mail them")
	}
	if *rules[0].Recovery {
		t.Fatal("Split changed the caller's config")
	}
	if _, _, unknown := Split(rules, []string{"a", "nope"}); len(unknown) != 1 || unknown[0] != "nope" {
		t.Fatalf("unknown = %v", unknown)
	}
}

// The incident this tool was built for: the vacuum gives up on its way home
// without an error code and sleeps off the dock.
func TestRunReplaysTheStuckVacuum(t *testing.T) {
	var js = `[{"name":"stuck","type":"state","topics":["home/roborock/+/status"],
		"condition":{"field":"state","regex":"^(idle|paused|error|charger_disconnected)$"},"for":"15m",
		"title":"Roborock {device} is stuck"}]`
	var rules []config.RuleConfig
	if err := json.Unmarshal([]byte(js), &rules); err != nil {
		t.Fatal(err)
	}
	status := func(sec int, state string) Message {
		return msg(sec, "home/roborock/carmen-og/status", `{"state":"`+state+`","error_code":0}`)
	}
	var msgs []Message
	for s := 0; s < 2*3600; s += 30 {
		state := "charging"
		switch {
		case s < 30*60:
			state = "segment_cleaning"
		case s < 35*60:
			state = "returning_home"
		case s < 45*60:
			state = "idle"
		case s < 100*60:
			state = "charger_disconnected"
		}
		msgs = append(msgs, status(s, state))
	}

	res, err := Run(rules, Slice(msgs), t0, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 2 || res.Events[0].Kind != mail.EventFiring || res.Events[1].Kind != mail.EventResolved {
		t.Fatalf("events: %+v", res.Events)
	}
	if fired := res.Events[0].At.Sub(t0); fired != 50*time.Minute {
		t.Fatalf("fired after %s, want 50m (35m gave up + 15m)", fired)
	}
	if res.Events[0].Alert.Title != "Roborock carmen-og is stuck" {
		t.Fatalf("title %q", res.Events[0].Alert.Title)
	}
	s := res.Rules[0]
	if s.Alerts != 1 || s.Watched != 1 || s.Longest != 50*time.Minute || s.StillFiring != 0 {
		t.Fatalf("summary %+v", s)
	}
}

// segmentLogger serves /api/segments and /api/segments/{day}/raw like
// mqtt-logger >= v1.6.0: finished days gzip, today plain with a half-written
// last line. With legacy set it answers those paths with its web UI instead,
// as older versions do, and only /api/events works.
type segmentLogger struct {
	fakeLogger
	legacy bool
	raw    atomic.Int32
}

func (f *segmentLogger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/segments") {
		f.fakeLogger.ServeHTTP(w, r)
		return
	}
	if f.legacy {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<!doctype html><div id=root></div>"))
		return
	}
	days := map[string][]Message{}
	for _, m := range f.events {
		d := m.Time.Format("2006-01-02")
		days[d] = append(days[d], m)
	}
	if r.URL.Path == "/api/segments" {
		var list []map[string]string
		for d := range days {
			list = append(list, map[string]string{"day": d})
		}
		json.NewEncoder(w).Encode(map[string]any{"segments": list})
		return
	}
	f.raw.Add(1)
	day := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/segments/"), "/raw")
	var buf strings.Builder
	for _, m := range days[day] {
		msg := json.RawMessage(m.Payload)
		if !json.Valid(m.Payload) {
			msg, _ = json.Marshal(string(m.Payload))
		}
		line, _ := json.Marshal(map[string]any{"timestamp": m.Time.Format(time.RFC3339), "topic": m.Topic, "message": msg})
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if day == "2026-09-22" { // "today": still being written
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write([]byte(buf.String() + `{"timestamp":"2026-09-22T10:00:00Z","topic":"a/x","mess`))
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	gz := gzip.NewWriter(w)
	gz.Write([]byte(buf.String()))
	gz.Close()
}

func twoDays() []Message {
	d1 := time.Date(2026, 9, 20, 23, 59, 0, 0, time.UTC) // crosses midnight into the 21st
	return []Message{
		{Time: d1, Topic: "a/bridge/state", Payload: []byte("offline")},
		{Time: d1.Add(30 * time.Second), Topic: "other/thing", Payload: []byte("ignored")},
		{Time: d1.Add(2 * time.Minute), Topic: "a/status", Payload: []byte(`{"state":"idle"}`)},
		{Time: d1.Add(3 * time.Minute), Topic: "a/bridge/state", Payload: []byte("online")},
	}
}

func TestStreamReadsDayFiles(t *testing.T) {
	fake := &segmentLogger{fakeLogger: fakeLogger{events: twoDays()}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	h := &History{BaseURL: srv.URL, Client: srv.Client()}
	var got []Message
	stats, err := h.Stream(context.Background(), []string{"a/#"}, t0.Add(-24*time.Hour), t0.Add(48*time.Hour), func(m Message) { got = append(got, m) })
	if err != nil {
		t.Fatal(err)
	}
	if stats.Mode != "segments" || fake.raw.Load() != 2 || fake.requests.Load() != 0 {
		t.Fatalf("mode %s, %d day files, %d query requests", stats.Mode, fake.raw.Load(), fake.requests.Load())
	}
	want := []string{"offline", `{"state":"idle"}`, "online"} // across midnight, in order, without other/thing
	if len(got) != len(want) {
		t.Fatalf("got %d messages: %+v", len(got), got)
	}
	for i, w := range want {
		if string(got[i].Payload) != w {
			t.Errorf("message %d = %q, want %q", i, got[i].Payload, w)
		}
	}
	if stats.Delivered != 3 || stats.Scanned != 4 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestStreamSkipsTodaysHalfWrittenLine(t *testing.T) {
	today := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)
	fake := &segmentLogger{fakeLogger: fakeLogger{events: []Message{{Time: today, Topic: "a/x", Payload: []byte("1")}}}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	h := &History{BaseURL: srv.URL, Client: srv.Client()}
	var got []Message
	stats, err := h.Stream(context.Background(), []string{"a/#"}, today.Add(-time.Hour), today.Add(2*time.Hour), func(m Message) { got = append(got, m) })
	if err != nil || len(got) != 1 || stats.Unreadable != 1 {
		t.Fatalf("got %d, stats %+v, err %v", len(got), stats, err)
	}
}

// An mqtt-logger older than v1.6.0 answers the day-file path with its web UI.
func TestStreamFallsBackToTheQueryAPI(t *testing.T) {
	fake := &segmentLogger{fakeLogger: fakeLogger{events: twoDays()}, legacy: true}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	h := &History{BaseURL: srv.URL, Client: srv.Client()}
	var got []Message
	stats, err := h.Stream(context.Background(), []string{"a/#"}, t0.Add(-24*time.Hour), t0.Add(48*time.Hour), func(m Message) { got = append(got, m) })
	if err != nil || stats.Mode != "events" || len(got) != 3 {
		t.Fatalf("mode %s, got %d, err %v", stats.Mode, len(got), err)
	}
}

// Both ways of reading must give the engine the same messages.
func TestDayFilesAndQueryAPIAgree(t *testing.T) {
	fake := &segmentLogger{fakeLogger: fakeLogger{events: twoDays()}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	read := func(queryOnly bool) []string {
		h := &History{BaseURL: srv.URL, Client: srv.Client(), QueryAPIOnly: queryOnly}
		var out []string
		if _, err := h.Stream(context.Background(), []string{"a/#"}, t0.Add(-24*time.Hour), t0.Add(48*time.Hour), func(m Message) {
			out = append(out, m.Time.Format(time.RFC3339)+" "+m.Topic+" "+string(m.Payload))
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	a, b := read(false), read(true)
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("day files:\n%s\nquery API:\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
}

// mqtt-logger >= v1.7.0 answers 429 while other queries run: wait and ask again.
func TestFetchWaitsOutTooManyRequests(t *testing.T) {
	fake := &fakeLogger{events: []Message{msg(0, "a/b", "x")}}
	busy := atomic.Int32{}
	busy.Store(2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if busy.Load() > 0 {
			busy.Add(-1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"too many queries at once"}`))
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer srv.Close()

	h := &History{BaseURL: srv.URL, Client: srv.Client()}
	got, err := h.Fetch(context.Background(), []string{"a/#"}, t0, t0.Add(time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d, err %v", len(got), err)
	}
}
