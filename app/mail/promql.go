package mail

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mqtt-home/mqtt-alerting/config"
)

// Sample is one series a PromQL query returned.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// PromQLRule is what the poller needs to know about a rule.
type PromQLRule struct {
	Name  string
	Query string
	// Watch returns the population of the rule; empty if not configured.
	Watch string
}

// unreachableRule is the name of the built-in alert for a Prometheus that does
// not answer. Without it, broken monitoring would look exactly like a healthy
// node: no series, no alert.
const unreachableRule = "prometheus-unreachable"

const unreachableTopic = "prometheus"

// blindRule is the built-in alert for a rule whose watch query returns nothing.
// A threshold query is silent when all is well and equally silent when its
// metric is gone - exporter down, metric renamed, dropped at scrape time.
const blindRule = "promql-rule-blind"

func builtinRule(name string) bool { return name == unreachableRule || name == blindRule }

// plumbingLabels say how a sample got into Prometheus, not what it is about.
// They are left out of the alert identity, so an alert survives a restarted
// exporter pod, and out of the default title.
var plumbingLabels = map[string]bool{
	"__name__": true, "job": true, "instance": true, "endpoint": true, "service": true,
	"container": true, "uid": true, "prometheus": true, "prometheus_replica": true,
}

// PromQLRules lists the rules the poller has to evaluate.
func (e *Engine) PromQLRules() []PromQLRule {
	var out []PromQLRule
	for _, r := range e.rules {
		if r.cfg.Type == config.RulePromQL && !builtinRule(r.Name()) {
			out = append(out, PromQLRule{Name: r.Name(), Query: r.cfg.Query, Watch: r.cfg.Watch})
		}
	}
	return out
}

func (e *Engine) rule(name string) *Rule {
	for _, r := range e.rules {
		if r.Name() == name {
			return r
		}
	}
	return nil
}

// seriesKey is the identity of a series: its meaningful labels, in a stable order.
func seriesKey(labels map[string]string) (key, device string) {
	var names []string
	for k := range labels {
		if !plumbingLabels[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var pairs, values []string
	for _, k := range names {
		pairs = append(pairs, k+"="+strconv.Quote(labels[k]))
		values = append(values, labels[k])
	}
	if len(pairs) == 0 {
		// Nothing but plumbing ("up == 0", "node_load15 > 8"): then the plumbing
		// is what tells the series apart, and leaving it out would merge every
		// down target into a single alert.
		for _, k := range []string{"job", "instance"} {
			if v, ok := labels[k]; ok {
				pairs = append(pairs, k+"="+strconv.Quote(v))
				values = append(values, v)
			}
		}
	}
	return "{" + strings.Join(pairs, ", ") + "}", strings.Join(values, " / ")
}

func formatSample(v float64) string {
	return strconv.FormatFloat(roundTo(v, 1), 'f', -1, 64)
}

func roundTo(v float64, decimals int) float64 {
	p := 1.0
	for i := 0; i < decimals; i++ {
		p *= 10
	}
	if v < 0 {
		return float64(int64(v*p-0.5)) / p
	}
	return float64(int64(v*p+0.5)) / p
}

// HandleQueryResult takes the answer to one rule's query. Every series in it is
// a condition that holds right now; every series that was there before and is
// gone has stopped holding.
func (e *Engine) HandleQueryResult(ruleName string, samples []Sample, now time.Time) {
	r := e.rule(ruleName)
	if r == nil || r.cfg.Type != config.RulePromQL {
		return
	}

	var events []Event
	e.mu.Lock()
	e.lastMessage = now // an answering Prometheus counts as input for the liveness probe

	seen := map[string]bool{}
	for _, sample := range samples {
		key, device := seriesKey(sample.Labels)
		if device == "" {
			device = ruleName
		}
		seen[key] = true
		inst := e.instanceFor(r, key, device)
		inst.labels = sample.Labels
		inst.value = formatSample(sample.Value)
		if inst.matchSince.IsZero() {
			inst.matchSince = now
		}
	}

	for k, inst := range e.instances {
		if inst.rule != r || seen[inst.topic] {
			continue
		}
		if inst.firing {
			events = append(events, e.resolve(inst, now))
		}
		// Series come and go (every failed job has its own name): forget what is
		// no longer reported, or the instance map grows forever.
		delete(e.instances, k)
	}
	e.mu.Unlock()

	e.setUnreachable("", now)
	events = append(events, e.evaluate(now)...)
	e.emit(events)
}

// HandleWatchResult takes the size of a rule's population. Zero means the rule
// is blind: whatever it is supposed to catch, it cannot see it any more.
func (e *Engine) HandleWatchResult(ruleName string, series int, now time.Time) {
	r := e.rule(ruleName)
	blind := e.rule(blindRule)
	if r == nil || blind == nil {
		return
	}

	var events []Event
	e.mu.Lock()
	e.watched[ruleName] = series
	inst := e.instanceFor(blind, ruleName, ruleName)
	if series > 0 {
		inst.matchSince = time.Time{}
		if inst.firing {
			events = append(events, e.resolve(inst, now))
		}
	} else {
		inst.value = "watch query returns no series"
		if inst.matchSince.IsZero() {
			inst.matchSince = now
		}
	}
	e.mu.Unlock()

	events = append(events, e.evaluate(now)...)
	e.emit(events)
}

// HandleQueryError records that Prometheus did not answer. It deliberately
// leaves every existing alert alone: not knowing is not the same as being fine,
// and resolving "disk full" because the monitoring died would be the worst
// possible moment to go quiet.
func (e *Engine) HandleQueryError(err error, now time.Time) {
	e.setUnreachable(err.Error(), now)
	e.emit(e.evaluate(now))
}

func (e *Engine) setUnreachable(reason string, now time.Time) {
	r := e.rule(unreachableRule)
	if r == nil {
		return
	}
	var events []Event
	e.mu.Lock()
	inst := e.instanceFor(r, unreachableTopic, "Prometheus")
	if reason == "" {
		inst.matchSince = time.Time{}
		if inst.firing {
			events = append(events, e.resolve(inst, now))
		}
	} else {
		if len(reason) > 160 {
			reason = reason[:160] + "…"
		}
		inst.value = reason
		if inst.matchSince.IsZero() {
			inst.matchSince = now
		}
	}
	e.mu.Unlock()
	e.emit(events)
}

// withBuiltinRules appends the built-in alerts that promql rules need.
func withBuiltinRules(cfgs []config.RuleConfig) []config.RuleConfig {
	hasPromQL, hasWatch := false, false
	configured := map[string]bool{}
	for _, c := range cfgs {
		if c.Type == config.RulePromQL {
			hasPromQL = true
			if c.Watch != "" {
				hasWatch = true
			}
		}
		configured[c.Name] = true // a built-in can be configured by hand, e.g. with a different `for`
	}

	out := append([]config.RuleConfig{}, cfgs...)
	if hasPromQL && !configured[unreachableRule] {
		out = append(out, config.RuleConfig{
			Name:          unreachableRule,
			Description:   "the promql rules cannot be evaluated; existing alerts stay as they are",
			Title:         "Prometheus does not answer",
			ResolvedTitle: "Prometheus answers again",
			Type:          config.RulePromQL,
			Query:         "(built in)",
			For:           config.Duration(10 * time.Minute),
			Repeat:        config.Duration(24 * time.Hour),
		})
	}
	if hasWatch && !configured[blindRule] {
		out = append(out, config.RuleConfig{
			Name:          blindRule,
			Description:   "the metric a rule depends on is gone (exporter down, metric renamed or dropped), so the rule can never fire",
			Title:         "Rule {device} sees no data",
			ResolvedTitle: "Rule {device} sees data again",
			Type:          config.RulePromQL,
			Query:         "(built in)",
			For:           config.Duration(15 * time.Minute),
			Repeat:        config.Duration(24 * time.Hour),
		})
	}
	return out
}
