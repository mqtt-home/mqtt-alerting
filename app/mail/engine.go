package mail

import (
	"sort"
	"sync"
	"time"

	"github.com/mqtt-home/mqtt-mail/config"
	"github.com/philipparndt/go-logger"
)

// Alert lifecycle.
const (
	StatePending = "pending"
	StateFiring  = "firing"
)

// Event kinds handed to the notifier.
const (
	EventFiring   = "firing"
	EventResolved = "resolved"
	EventReminder = "reminder"
)

// Alert is one rule evaluated against one concrete topic.
// Keep the JSON tags snake_case — the whole house reads these payloads.
type Alert struct {
	Rule string `json:"rule"`
	// Title is the human readable headline ("haus/shelly is offline").
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Check is the rule in words ("payload = \"offline\" for 10m").
	Check   string     `json:"check,omitempty"`
	Type    string     `json:"type"`
	Topic   string     `json:"topic"`
	State   string     `json:"state"`
	Value   string     `json:"value,omitempty"`
	Since   time.Time  `json:"since"`
	FiredAt *time.Time `json:"fired_at,omitempty"`
}

// Event is an alert transition worth telling someone about.
type Event struct {
	Kind  string    `json:"kind"`
	At    time.Time `json:"at"`
	Alert Alert     `json:"alert"`
	// Duration is how long the alert had been firing, on resolved/reminder.
	Duration string `json:"duration,omitempty"`
}

type RuleInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`
	Topics      []string `json:"topics"`
	Summary     string   `json:"summary"`
	Watching    int      `json:"watching"`
	Firing      int      `json:"firing"`
	Pending     int      `json:"pending"`
}

type MailStats struct {
	Enabled      bool       `json:"enabled"`
	Sent         int        `json:"sent"`
	Queued       int        `json:"queued"`
	LastSentAt   *time.Time `json:"last_sent_at,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	LastErrorAt  *time.Time `json:"last_error_at,omitempty"`
	SentLastHour int        `json:"sent_last_hour"`
}

// Status is what gets published to `<topic>/status` and pushed over SSE.
type Status struct {
	Online    bool      `json:"online"`
	UpdatedAt time.Time `json:"updated_at"`
	StartedAt time.Time `json:"started_at"`
	Messages  int64     `json:"messages"`
	// Dropped counts messages discarded because the engine could not keep up.
	// Anything but zero deserves a look.
	Dropped int64      `json:"dropped"`
	Rules   []RuleInfo `json:"rules"`
	Alerts  []Alert    `json:"alerts"`
	History []Event    `json:"history"`
	Mail    MailStats  `json:"mail"`
}

type StatusListener func(Status)

// instance is the mutable per-(rule, topic) state behind an Alert.
type instance struct {
	rule *Rule
	// topic is the concrete topic, or for a group rule the filter it stands for.
	topic  string
	device string

	// count: last matching payload, to recognise a duplicate delivery
	lastHit   string
	lastHitAt time.Time

	// state / count: when the condition started to hold (zero: it doesn't)
	matchSince time.Time
	value      string
	// count: timestamps of matches inside the window
	hits []time.Time
	// silence: last message seen (or when we started waiting for one)
	lastSeen time.Time

	firing       bool
	firedAt      time.Time
	lastNotified time.Time
}

const historySize = 50

// Engine evaluates the rules. It knows nothing about MQTT or SMTP: messages go
// in through HandleMessage, time passes through Tick, events come out of the
// listener. That keeps it testable with a fake clock.
type Engine struct {
	rules []*Rule

	mu        sync.Mutex
	instances map[string]*instance
	history   []Event
	messages  int64
	startedAt time.Time
	// liveness: when the engine last ticked and last saw a message
	lastTick    time.Time
	lastMessage time.Time

	onEvent   func(Event)
	listeners []StatusListener
	mailStats func() MailStats
	dropped   func() int64
}

func NewEngine(cfgs []config.RuleConfig, now time.Time) (*Engine, error) {
	rules, err := compileRules(cfgs)
	if err != nil {
		return nil, err
	}

	e := &Engine{
		rules:     rules,
		instances: map[string]*instance{},
		startedAt: now,
	}

	// A silence rule on a concrete topic has to fire even if that topic never
	// says a word, so its clock starts now. Wildcard filters can only be
	// tracked once a topic has shown up.
	for _, r := range rules {
		if r.cfg.Type != config.RuleSilence {
			continue
		}
		for _, f := range r.cfg.Topics {
			switch {
			case r.cfg.Group:
				// The whole filter is one thing to watch, and "never heard
				// anything" is the failure we are after.
				e.instanceFor(r, f, groupName(f)).lastSeen = now
			case isConcrete(f) && r.appliesTo(f):
				e.instanceFor(r, f, deviceName(f, f)).lastSeen = now
			}
		}
	}

	return e, nil
}

func (e *Engine) OnEvent(f func(Event))                    { e.onEvent = f }
func (e *Engine) AddStatusChangeListener(l StatusListener) { e.listeners = append(e.listeners, l) }
func (e *Engine) SetMailStats(f func() MailStats)          { e.mailStats = f }
func (e *Engine) SetDroppedCounter(f func() int64)         { e.dropped = f }

// Subscriptions returns the MQTT filters to subscribe to, with filters that
// another one already covers removed — overlapping subscriptions would deliver
// the same message twice and double every count.
func (e *Engine) Subscriptions() []string {
	var all []string
	seen := map[string]bool{}
	for _, r := range e.rules {
		for _, f := range r.cfg.Topics {
			if !seen[f] {
				seen[f] = true
				all = append(all, f)
			}
		}
	}

	var out []string
	for i, f := range all {
		covered := false
		for j, other := range all {
			if i != j && filterCovers(other, f) && !(filterCovers(f, other) && j > i) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

func key(rule, topic string) string { return rule + "\x00" + topic }

func (e *Engine) instanceFor(r *Rule, topic, device string) *instance {
	k := key(r.Name(), topic)
	inst, ok := e.instances[k]
	if !ok {
		inst = &instance{rule: r, topic: topic, device: device}
		e.instances[k] = inst
	}
	return inst
}

// duplicateWindow is how close two identical payloads have to be to count as
// one. Overlapping subscriptions ("+/+/bridge/logs" and "haus/unifi-access/#")
// can make the broker deliver a message once per subscription, which would
// double every count.
const duplicateWindow = 250 * time.Millisecond

// HandleMessage feeds one MQTT message through every rule that watches it.
func (e *Engine) HandleMessage(topic string, payload []byte, now time.Time) {
	var events []Event

	e.mu.Lock()
	e.messages++
	e.lastMessage = now

	// An empty payload is how a retained message gets deleted: the thing is
	// gone for good (a device that was removed), not merely fine again. Forget
	// it, otherwise its last state would be watched and reported forever.
	if len(payload) == 0 {
		for k, inst := range e.instances {
			if inst.topic != topic || inst.rule.cfg.Group {
				continue
			}
			if inst.firing {
				events = append(events, e.resolve(inst, now))
			}
			delete(e.instances, k)
		}
		e.mu.Unlock()
		e.emit(events)
		return
	}

	for _, r := range e.rules {
		filter, ok := r.matchedFilter(topic)
		if !ok {
			continue
		}
		var inst *instance
		if r.cfg.Group {
			inst = e.instanceFor(r, filter, groupName(filter))
		} else {
			inst = e.instanceFor(r, topic, deviceName(filter, topic))
		}

		switch r.cfg.Type {
		case config.RuleSilence:
			inst.lastSeen = now
			if inst.firing {
				events = append(events, e.resolve(inst, now))
			}

		case config.RuleState:
			ok, value := r.matches(payload)
			if ok {
				if inst.matchSince.IsZero() {
					inst.matchSince = now
				}
				inst.value = value
			} else {
				inst.matchSince = time.Time{}
				if inst.firing {
					inst.value = value
					events = append(events, e.resolve(inst, now))
				}
			}

		case config.RuleCount:
			if ok, value := r.matches(payload); ok {
				raw := string(payload)
				if raw == inst.lastHit && now.Sub(inst.lastHitAt) < duplicateWindow {
					continue
				}
				inst.lastHit, inst.lastHitAt = raw, now
				inst.hits = append(inst.hits, now)
				inst.value = value
			}
		}
	}
	e.mu.Unlock()

	// Conditions with for=0 and counts fire right here rather than a tick later.
	events = append(events, e.evaluate(now)...)
	e.emit(events)
}

// Tick advances time: pending alerts fire, silent topics are noticed, counts
// expire, reminders go out.
func (e *Engine) Tick(now time.Time) {
	e.mu.Lock()
	e.lastTick = now
	e.mu.Unlock()
	e.emit(e.evaluate(now))
}

func (e *Engine) evaluate(now time.Time) []Event {
	var events []Event

	e.mu.Lock()
	defer e.mu.Unlock()

	for _, inst := range e.instances {
		r := inst.rule
		shouldFire := false

		switch r.cfg.Type {
		case config.RuleState:
			shouldFire = !inst.matchSince.IsZero() && now.Sub(inst.matchSince) >= r.cfg.For.Std()

		case config.RuleSilence:
			shouldFire = !inst.lastSeen.IsZero() && now.Sub(inst.lastSeen) >= r.cfg.For.Std()
			if shouldFire {
				inst.value = "no message for " + humanDuration(now.Sub(inst.lastSeen))
			}

		case config.RuleCount:
			cutoff := now.Add(-r.cfg.Within.Std())
			keep := inst.hits[:0]
			for _, h := range inst.hits {
				if h.After(cutoff) {
					keep = append(keep, h)
				}
			}
			inst.hits = keep
			if len(inst.hits) >= r.cfg.Count {
				shouldFire = true
			} else if inst.firing && len(inst.hits) > 0 {
				// Still happening, just slower: resolve only once it has been
				// quiet for a whole window, otherwise the alert flaps.
				shouldFire = true
			}
		}

		switch {
		case shouldFire && !inst.firing:
			inst.firing = true
			inst.firedAt = now
			inst.lastNotified = now
			events = append(events, e.record(Event{Kind: EventFiring, At: now, Alert: e.alertOf(inst)}))

		case !shouldFire && inst.firing:
			events = append(events, e.resolve(inst, now))

		case inst.firing && r.cfg.Repeat > 0 && now.Sub(inst.lastNotified) >= r.cfg.Repeat.Std():
			inst.lastNotified = now
			events = append(events, e.record(Event{
				Kind: EventReminder, At: now, Alert: e.alertOf(inst),
				Duration: humanDuration(now.Sub(inst.firedAt)),
			}))
		}
	}

	return events
}

// resolve must be called with e.mu held.
func (e *Engine) resolve(inst *instance, now time.Time) Event {
	ev := Event{
		Kind: EventResolved, At: now, Alert: e.alertOf(inst),
		Duration: humanDuration(now.Sub(inst.firedAt)),
	}
	ev.Alert.Title = inst.rule.resolvedTitle(inst.device, inst.topic, inst.value)
	if inst.rule.cfg.Type != config.RuleState {
		// For a state rule the value is what the topic says *now* and worth
		// showing. "no message for 47m" on a recovery is just confusing.
		ev.Alert.Value = ""
	}
	inst.firing = false
	inst.firedAt = time.Time{}
	inst.matchSince = time.Time{}
	return e.record(ev)
}

// record must be called with e.mu held.
func (e *Engine) record(ev Event) Event {
	e.history = append([]Event{ev}, e.history...)
	if len(e.history) > historySize {
		e.history = e.history[:historySize]
	}
	return ev
}

func (e *Engine) alertOf(inst *instance) Alert {
	a := Alert{
		Rule:        inst.rule.Name(),
		Title:       inst.rule.title(inst.device, inst.topic, inst.value),
		Description: inst.rule.cfg.Description,
		Check:       inst.rule.summary(),
		Type:        inst.rule.cfg.Type,
		Topic:       inst.topic,
		Value:       inst.value,
		State:       StatePending,
		Since:       inst.matchSince,
	}
	if inst.rule.cfg.Type == config.RuleSilence {
		a.Since = inst.lastSeen
	}
	if inst.rule.cfg.Type == config.RuleCount && len(inst.hits) > 0 {
		a.Since = inst.hits[0]
	}
	if inst.firing {
		a.State = StateFiring
		t := inst.firedAt
		a.FiredAt = &t
	}
	return a
}

func (e *Engine) emit(events []Event) {
	if len(events) == 0 {
		return
	}
	for _, ev := range events {
		logger.Info("Alert "+ev.Kind, "rule", ev.Alert.Rule, "topic", ev.Alert.Topic, "value", ev.Alert.Value)
		if e.onEvent != nil && (ev.Kind != EventResolved || e.recoveryWanted(ev.Alert.Rule)) {
			e.onEvent(ev)
		}
	}
	e.NotifyStatus()
}

func (e *Engine) recoveryWanted(rule string) bool {
	for _, r := range e.rules {
		if r.Name() == rule {
			return r.recovery
		}
	}
	return true
}

// NotifyStatus pushes the current status to MQTT and the web UI.
func (e *Engine) NotifyStatus() {
	status := e.GetStatus()
	for _, l := range e.listeners {
		l(status)
	}
}

func (e *Engine) GetStatus() Status {
	now := time.Now()

	e.mu.Lock()
	status := Status{
		Online:    true,
		UpdatedAt: now,
		StartedAt: e.startedAt,
		Messages:  e.messages,
		Alerts:    []Alert{},
		History:   append([]Event{}, e.history...),
	}

	watching := map[string]int{}
	firing := map[string]int{}
	pendingCount := map[string]int{}
	for _, inst := range e.instances {
		watching[inst.rule.Name()]++
		pending := !inst.matchSince.IsZero() && inst.rule.cfg.Type == config.RuleState
		if inst.firing {
			firing[inst.rule.Name()]++
		} else if pending {
			pendingCount[inst.rule.Name()]++
		}
		if inst.firing || pending {
			status.Alerts = append(status.Alerts, e.alertOf(inst))
		}
	}
	for _, r := range e.rules {
		status.Rules = append(status.Rules, RuleInfo{
			Name:        r.Name(),
			Description: r.cfg.Description,
			Type:        r.cfg.Type,
			Topics:      r.cfg.Topics,
			Summary:     r.summary(),
			Watching:    watching[r.Name()],
			Firing:      firing[r.Name()],
			Pending:     pendingCount[r.Name()],
		})
	}
	e.mu.Unlock()

	sort.Slice(status.Alerts, func(i, j int) bool {
		a, b := status.Alerts[i], status.Alerts[j]
		if a.State != b.State {
			return a.State == StateFiring
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Topic < b.Topic
	})

	if e.mailStats != nil {
		status.Mail = e.mailStats()
	}
	if e.dropped != nil {
		status.Dropped = e.dropped()
	}
	return status
}

// How long the engine may go without ticking / without any message before it
// counts as stuck. Every watched service publishes at least every few minutes,
// so total silence means our own subscription is dead, not the house.
const (
	maxTickAge    = time.Minute
	maxMessageAge = 10 * time.Minute
)

// IsConnected feeds the liveness probe: the engine is alive if it still ticks
// and still hears something. A monitor that has silently stopped is worse than
// none, so it has to be able to notice that about itself and get restarted.
// (A broken SMTP account is reported in the status instead — a restart would
// not fix that.)
func (e *Engine) IsConnected() bool {
	return e.healthyAt(time.Now())
}

func (e *Engine) healthyAt(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	lastTick, lastMessage := e.lastTick, e.lastMessage
	if lastTick.IsZero() {
		lastTick = e.startedAt
	}
	if lastMessage.IsZero() {
		lastMessage = e.startedAt
	}
	if len(e.rules) == 0 {
		lastMessage = now // nothing subscribed, nothing to hear
	}
	return now.Sub(lastTick) < maxTickAge && now.Sub(lastMessage) < maxMessageAge
}

// Run ticks until stop is closed.
func (e *Engine) Run(interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			e.Tick(now)
		case <-stop:
			return
		}
	}
}
