package mail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const diskRule = `[{
	"name": "disk-full", "type": "promql",
	"query": "100 - node_filesystem_avail_bytes / node_filesystem_size_bytes * 100 > 85",
	"title": "Disk {mountpoint} is {value} % full", "resolved_title": "Disk {mountpoint} has room again",
	"for": "15m", "repeat": "24h"
}]`

func disk(mount string, v float64) Sample {
	return Sample{Labels: map[string]string{"mountpoint": mount, "instance": "10.10.1.1:9100", "job": "node-exporter"}, Value: v}
}

func TestPromQLFiresPerSeriesAfterFor(t *testing.T) {
	e, got := newEngine(t, diskRule)

	e.HandleQueryResult("disk-full", []Sample{disk("/", 91.26), disk("/data", 88)}, t0)
	e.HandleQueryResult("disk-full", []Sample{disk("/", 91.31), disk("/data", 88)}, t0.Add(10*time.Minute))
	if len(*got) != 0 {
		t.Fatalf("fired before `for`: %s", kinds(*got))
	}

	e.HandleQueryResult("disk-full", []Sample{disk("/", 92.04)}, t0.Add(15*time.Minute))
	if len(*got) != 1 {
		t.Fatalf("got %s", kinds(*got))
	}
	a := (*got)[0].Alert
	if a.Title != "Disk / is 92 % full" || a.Value != "92" {
		t.Fatalf("title %q value %q", a.Title, a.Value)
	}
	// The identity ignores plumbing labels, so a restarted exporter is the same alert.
	if a.Topic != `{mountpoint="/"}` {
		t.Fatalf("series key = %q", a.Topic)
	}
	// /data dropped out of the result before its 15 minutes were up: never fired.
	for _, al := range e.GetStatus().Alerts {
		if strings.Contains(al.Topic, "/data") {
			t.Fatalf("/data still tracked: %+v", al)
		}
	}
}

func TestPromQLResolvesWhenTheSeriesIsGone(t *testing.T) {
	e, got := newEngine(t, diskRule)
	e.HandleQueryResult("disk-full", []Sample{disk("/", 95)}, t0)
	e.HandleQueryResult("disk-full", []Sample{disk("/", 95)}, t0.Add(20*time.Minute))
	e.HandleQueryResult("disk-full", nil, t0.Add(50*time.Minute))

	last := (*got)[len(*got)-1]
	if last.Kind != EventResolved || last.Alert.Title != "Disk / has room again" || last.Duration != "30m" {
		t.Fatalf("got %+v", last)
	}
	if n := len(e.GetStatus().Alerts); n != 0 {
		t.Fatalf("%d alerts left", n)
	}
}

// Monitoring that died must not look like a healthy node.
func TestPromQLFailureKeepsAlertsAndRaisesItsOwn(t *testing.T) {
	e, got := newEngine(t, diskRule)
	e.HandleQueryResult("disk-full", []Sample{disk("/", 95)}, t0)
	e.HandleQueryResult("disk-full", []Sample{disk("/", 95)}, t0.Add(15*time.Minute))

	down := errors.New("dial tcp: connection refused")
	e.HandleQueryError(down, t0.Add(16*time.Minute))
	e.HandleQueryError(down, t0.Add(20*time.Minute))
	if kinds(*got) != `firing:{mountpoint="/"}` {
		t.Fatalf("a short outage changed something: %s", kinds(*got))
	}

	e.HandleQueryError(down, t0.Add(27*time.Minute))
	last := (*got)[len(*got)-1]
	if last.Alert.Rule != unreachableRule || last.Alert.Title != "Prometheus does not answer" || !strings.Contains(last.Alert.Value, "connection refused") {
		t.Fatalf("got %+v", last.Alert)
	}
	firing := 0
	for _, a := range e.GetStatus().Alerts {
		if a.State == StateFiring {
			firing++
		}
	}
	if firing != 2 {
		t.Fatalf("disk alert must survive the outage, firing=%d", firing)
	}

	e.HandleQueryResult("disk-full", []Sample{disk("/", 95)}, t0.Add(30*time.Minute))
	last = (*got)[len(*got)-1]
	if last.Kind != EventResolved || last.Alert.Title != "Prometheus answers again" {
		t.Fatalf("got %+v", last)
	}
}

func TestUnreachableRuleOnlyExistsWithPromQLRules(t *testing.T) {
	e, _ := newEngine(t, offlineRule)
	for _, r := range e.GetStatus().Rules {
		if r.Name == unreachableRule {
			t.Fatal("built-in rule added without any promql rule")
		}
	}
	e, _ = newEngine(t, diskRule)
	rules := e.GetStatus().Rules
	if rules[len(rules)-1].Name != unreachableRule || rules[len(rules)-1].Watching != 1 {
		t.Fatalf("got %+v", rules)
	}
	if len(e.PromQLRules()) != 1 {
		t.Fatalf("the built-in rule must not be polled: %+v", e.PromQLRules())
	}
}

func TestPromQLRuleValidation(t *testing.T) {
	for name, js := range map[string]string{
		"no query":    `[{"name":"a","type":"promql"}]`,
		"with topics": `[{"name":"a","type":"promql","query":"up == 0","topics":["a/#"]}]`,
	} {
		if _, err := NewEngine(rules(t, js), t0); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestQueryPrometheus(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		switch {
		case strings.Contains(gotQuery, "bad("):
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error: unknown function"}`))
		case strings.Contains(gotQuery, "html"):
			w.Write([]byte(`<html>502 Bad Gateway</html>`))
		default:
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"mountpoint":"/","job":"node-exporter"},"value":[1758360000.1,"52.04"]}]}}`))
		}
	}))
	defer srv.Close()

	samples, err := QueryPrometheus(context.Background(), srv.Client(), srv.URL+"/", `100 - x{a="b"} > 85`)
	if err != nil || len(samples) != 1 || samples[0].Value != 52.04 || samples[0].Labels["mountpoint"] != "/" {
		t.Fatalf("samples=%+v err=%v", samples, err)
	}
	if gotQuery != `100 - x{a="b"} > 85` {
		t.Fatalf("query arrived as %q", gotQuery)
	}

	if _, err := QueryPrometheus(context.Background(), srv.Client(), srv.URL, "bad("); err == nil || !strings.Contains(err.Error(), "unknown function") {
		t.Fatalf("a rejected query must be an error, got %v", err)
	}
	if _, err := QueryPrometheus(context.Background(), srv.Client(), srv.URL, "html"); err == nil {
		t.Fatal("a proxy error page must not count as 'no series'")
	}
}

func TestPromQLSeriesWithOnlyPlumbingLabelsStayDistinct(t *testing.T) {
	e, got := newEngine(t, `[{"name": "target-down", "type": "promql", "query": "up == 0", "title": "{job} is down"}]`)
	e.HandleQueryResult("target-down", []Sample{
		{Labels: map[string]string{"job": "node-exporter", "instance": "10.10.1.1:9100"}},
		{Labels: map[string]string{"job": "coredns", "instance": "10.244.0.245:9153"}},
	}, t0)
	if len(*got) != 2 {
		t.Fatalf("two targets, got %d alerts: %s", len(*got), kinds(*got))
	}
	titles := (*got)[0].Alert.Title + "|" + (*got)[1].Alert.Title
	if !strings.Contains(titles, "node-exporter is down") || !strings.Contains(titles, "coredns is down") {
		t.Fatalf("titles: %s", titles)
	}
}

// The web UI iterates over these lists. A nil slice marshals to null, and one
// null took the whole page down.
func TestStatusJSONHasNoNullLists(t *testing.T) {
	e, _ := newEngine(t, diskRule)
	raw, err := json.Marshal(e.GetStatus())
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	json.Unmarshal(raw, &generic)
	for _, key := range []string{"rules", "alerts", "history"} {
		if generic[key] == nil {
			t.Errorf("%q is null", key)
		}
	}
	for _, r := range generic["rules"].([]any) {
		rule := r.(map[string]any)
		if rule["topics"] == nil {
			t.Errorf("rule %v: topics is null", rule["name"])
		}
	}
}

const watchedRule = `[{
	"name": "pod-not-running", "type": "promql",
	"query": "max by (namespace, pod) (kube_pod_status_phase{phase=\"Pending\"}) == 1",
	"watch": "max by (namespace, pod) (kube_pod_status_phase)",
	"title": "{namespace}/{pod} is stuck", "for": "15m"
}]`

func ruleInfo(t *testing.T, e *Engine, name string) RuleInfo {
	t.Helper()
	for _, r := range e.GetStatus().Rules {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("rule %q not in status", name)
	return RuleInfo{}
}

// "0 watched" on a healthy cluster looked like a broken rule. With a watch
// query the rule says what it covers and how much of it is fine.
func TestWatchQueryGivesPopulationAndOKCount(t *testing.T) {
	e, _ := newEngine(t, watchedRule)

	if info := ruleInfo(t, e, "pod-not-running"); !info.Blind || info.OK != 0 {
		t.Fatalf("before the first watch result the coverage is unknown: %+v", info)
	}

	e.HandleQueryResult("pod-not-running", nil, t0)
	e.HandleWatchResult("pod-not-running", 53, t0)
	if info := ruleInfo(t, e, "pod-not-running"); info.Blind || info.Watching != 53 || info.OK != 53 || info.Firing != 0 {
		t.Fatalf("healthy: %+v", info)
	}

	stuck := []Sample{{Labels: map[string]string{"namespace": "mqttbridge", "pod": "bambu-1"}, Value: 1}}
	e.HandleQueryResult("pod-not-running", stuck, t0.Add(time.Minute))
	if info := ruleInfo(t, e, "pod-not-running"); info.Pending != 1 || info.OK != 52 {
		t.Fatalf("one pending: %+v", info)
	}
	e.HandleQueryResult("pod-not-running", stuck, t0.Add(20*time.Minute))
	if info := ruleInfo(t, e, "pod-not-running"); info.Firing != 1 || info.OK != 52 || info.Watching != 53 {
		t.Fatalf("one firing: %+v", info)
	}
}

// The failure this exists for: the metric disappears, the threshold query keeps
// returning nothing, and everything looks perfectly healthy.
func TestBlindRuleFiresWhenThePopulationIsGone(t *testing.T) {
	e, got := newEngine(t, watchedRule)
	e.HandleWatchResult("pod-not-running", 53, t0)

	e.HandleWatchResult("pod-not-running", 0, t0.Add(time.Minute))
	e.HandleWatchResult("pod-not-running", 0, t0.Add(10*time.Minute))
	if len(*got) != 0 {
		t.Fatalf("a short gap must not alert: %s", kinds(*got))
	}

	e.HandleWatchResult("pod-not-running", 0, t0.Add(17*time.Minute))
	if len(*got) != 1 || (*got)[0].Alert.Rule != blindRule || (*got)[0].Alert.Title != "Rule pod-not-running sees no data" {
		t.Fatalf("got %+v", *got)
	}

	e.HandleWatchResult("pod-not-running", 48, t0.Add(20*time.Minute))
	last := (*got)[len(*got)-1]
	if last.Kind != EventResolved || last.Alert.Title != "Rule pod-not-running sees data again" {
		t.Fatalf("got %+v", last)
	}
}

func TestBlindRuleOnlyExistsWithWatchQueries(t *testing.T) {
	e, _ := newEngine(t, diskRule) // promql, but no watch
	for _, r := range e.GetStatus().Rules {
		if r.Name == blindRule {
			t.Fatal("built-in blind rule added although no rule has a watch query")
		}
	}
	if !ruleInfo(t, e, "disk-full").Blind {
		t.Fatal("a promql rule without watch must say that its coverage is unknown")
	}

	e, _ = newEngine(t, watchedRule)
	ruleInfo(t, e, blindRule)
	for _, q := range e.PromQLRules() {
		if builtinRule(q.Name) {
			t.Fatalf("built-in rule must not be polled: %+v", q)
		}
	}
}

func TestOKCountForMQTTRules(t *testing.T) {
	e, _ := newEngine(t, offlineRule)
	e.HandleMessage("sonos/bridge/state", []byte("online"), t0)
	e.HandleMessage("hue/bridge/state", []byte("online"), t0)
	e.HandleMessage("rules/bridge/state", []byte("offline"), t0)
	if info := ruleInfo(t, e, "bridge-offline"); info.Watching != 3 || info.OK != 2 || info.Pending != 1 {
		t.Fatalf("got %+v", info)
	}
}

func TestWatchOnlyForPromQL(t *testing.T) {
	_, err := NewEngine(rules(t, `[{"name":"a","type":"silence","topics":["a"],"for":"1m","watch":"up"}]`), t0)
	if err == nil {
		t.Fatal("accepted watch on a silence rule")
	}
}
