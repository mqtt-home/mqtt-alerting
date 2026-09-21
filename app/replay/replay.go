package replay

import (
	"sort"
	"time"

	"github.com/mqtt-home/mqtt-alerting/config"
	"github.com/mqtt-home/mqtt-alerting/mail"
)

// Tick is how often the running service evaluates time-based conditions.
const Tick = 5 * time.Second

// Split separates rules that can be replayed from promql rules, which need
// Prometheus history (and a different store) and are reported as skipped.
// only, if not empty, restricts the replay to these rule names.
func Split(rules []config.RuleConfig, only []string) (replayable []config.RuleConfig, skipped []string, unknown []string) {
	want := map[string]bool{}
	for _, n := range only {
		want[n] = true
	}
	seen := map[string]bool{}
	for _, r := range rules {
		seen[r.Name] = true
		if len(want) > 0 && !want[r.Name] {
			continue
		}
		if r.Type == config.RulePromQL {
			skipped = append(skipped, r.Name)
			continue
		}
		// A replay reports; it does not mail. Recoveries are part of the
		// report even for rules that do not send a "resolved" mail.
		on := true
		r.Recovery = &on
		replayable = append(replayable, r)
	}
	for n := range want {
		if !seen[n] {
			unknown = append(unknown, n)
		}
	}
	sort.Strings(unknown)
	return replayable, skipped, unknown
}

// RuleSummary is what one rule did over the replayed period.
type RuleSummary struct {
	Name      string
	Watched   int // distinct topics (or groups) the rule saw
	Alerts    int
	Reminders int
	Longest   time.Duration
	Total     time.Duration
	// StillFiring counts alerts that had not resolved when the period ended.
	StillFiring int
}

// Result is the outcome of a replay.
type Result struct {
	Events   []mail.Event
	Rules    []RuleSummary
	Messages int
}

// Run feeds messages through a fresh engine, advancing its clock the way the
// service does, and collects every alert transition. feed calls its argument
// once per message, in the order they were received.
func Run(rules []config.RuleConfig, feed func(func(Message)) error, from, to time.Time) (Result, error) {
	engine, err := mail.NewEngine(rules, from)
	if err != nil {
		return Result{}, err
	}
	var events []mail.Event
	engine.OnEvent(func(ev mail.Event) { events = append(events, ev) })

	clock := from
	advance := func(until time.Time) {
		for !clock.Add(Tick).After(until) {
			clock = clock.Add(Tick)
			engine.Tick(clock)
		}
	}
	count := 0
	err = feed(func(m Message) {
		if m.Time.Before(from) || m.Time.After(to) {
			return
		}
		count++
		advance(m.Time)
		engine.HandleMessage(m.Topic, m.Payload, m.Time)
	})
	if err != nil {
		return Result{}, err
	}
	advance(to)

	return Result{Events: events, Rules: summarize(rules, events, engine.GetStatus(), to), Messages: count}, nil
}

// Slice feeds a list of messages, for callers that already have them.
func Slice(msgs []Message) func(func(Message)) error {
	return func(fn func(Message)) error {
		for _, m := range msgs {
			fn(m)
		}
		return nil
	}
}

func summarize(rules []config.RuleConfig, events []mail.Event, status mail.Status, end time.Time) []RuleSummary {
	byName := map[string]*RuleSummary{}
	var order []string
	for _, r := range rules {
		byName[r.Name] = &RuleSummary{Name: r.Name}
		order = append(order, r.Name)
	}
	for _, r := range status.Rules {
		if s := byName[r.Name]; s != nil {
			s.Watched = r.Watching
		}
	}

	open := map[string]time.Time{} // rule + topic -> fired at
	for _, ev := range events {
		s := byName[ev.Alert.Rule]
		if s == nil {
			continue
		}
		key := ev.Alert.Rule + "\x00" + ev.Alert.Topic
		switch ev.Kind {
		case mail.EventFiring:
			s.Alerts++
			open[key] = ev.At
		case mail.EventReminder:
			s.Reminders++
		case mail.EventResolved:
			if at, ok := open[key]; ok {
				d := ev.At.Sub(at)
				s.Total += d
				if d > s.Longest {
					s.Longest = d
				}
				delete(open, key)
			}
		}
	}
	for key, at := range open {
		for _, s := range byName {
			if len(key) > len(s.Name) && key[:len(s.Name)+1] == s.Name+"\x00" {
				d := end.Sub(at)
				s.Total += d
				if d > s.Longest {
					s.Longest = d
				}
				s.StillFiring++
			}
		}
	}

	out := make([]RuleSummary, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out
}
