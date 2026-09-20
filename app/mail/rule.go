package mail

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mqtt-home/mqtt-alerting/config"
)

// Rule is a validated, compiled config.RuleConfig.
type Rule struct {
	cfg      config.RuleConfig
	regex    *regexp.Regexp
	recovery bool
}

func compileRules(cfgs []config.RuleConfig) ([]*Rule, error) {
	rules := make([]*Rule, 0, len(cfgs))
	seen := map[string]bool{}
	for i, rc := range cfgs {
		r, err := compileRule(rc)
		if err != nil {
			return nil, fmt.Errorf("rule #%d (%q): %w", i+1, rc.Name, err)
		}
		if seen[rc.Name] {
			return nil, fmt.Errorf("rule #%d: duplicate name %q", i+1, rc.Name)
		}
		seen[rc.Name] = true
		rules = append(rules, r)
	}
	return rules, nil
}

func compileRule(rc config.RuleConfig) (*Rule, error) {
	if rc.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if len(rc.Topics) == 0 && rc.Type != config.RulePromQL {
		return nil, fmt.Errorf("at least one topic is required")
	}

	r := &Rule{cfg: rc, recovery: rc.Recovery == nil || *rc.Recovery}

	switch rc.Type {
	case config.RuleState:
		if rc.Condition == nil {
			return nil, fmt.Errorf("type %q needs a condition", rc.Type)
		}
	case config.RuleCount:
		if rc.Condition == nil {
			return nil, fmt.Errorf("type %q needs a condition", rc.Type)
		}
		if rc.Count < 1 || rc.Within <= 0 {
			return nil, fmt.Errorf("type %q needs count >= 1 and within > 0", rc.Type)
		}
	case config.RuleSilence:
		if rc.For <= 0 {
			return nil, fmt.Errorf("type %q needs for > 0", rc.Type)
		}
	case config.RulePromQL:
		if strings.TrimSpace(rc.Query) == "" {
			return nil, fmt.Errorf("type %q needs a query", rc.Type)
		}
		if len(rc.Topics) > 0 || rc.Condition != nil {
			return nil, fmt.Errorf("type %q takes a query, not topics or a condition", rc.Type)
		}
	default:
		return nil, fmt.Errorf("unknown type %q (state, count, silence, promql)", rc.Type)
	}

	if rc.Watch != "" && rc.Type != config.RulePromQL {
		return nil, fmt.Errorf("watch is only valid for type %q", config.RulePromQL)
	}

	if rc.Group && rc.Type != config.RuleSilence {
		return nil, fmt.Errorf("group is only valid for type %q", config.RuleSilence)
	}

	if c := rc.Condition; c != nil {
		if c.Equals == nil && c.NotEquals == nil && c.Regex == "" && c.LT == nil && c.GT == nil {
			return nil, fmt.Errorf("condition has no operator")
		}
		if c.Regex != "" {
			re, err := regexp.Compile(c.Regex)
			if err != nil {
				return nil, fmt.Errorf("condition.regex: %w", err)
			}
			r.regex = re
		}
	}

	return r, nil
}

func (r *Rule) Name() string { return r.cfg.Name }

// appliesTo reports whether the rule watches this concrete topic.
func (r *Rule) appliesTo(topic string) bool {
	_, ok := r.matchedFilter(topic)
	return ok
}

// matchedFilter returns the first of the rule's filters that matches the topic.
func (r *Rule) matchedFilter(topic string) (string, bool) {
	for _, ex := range r.cfg.Exclude {
		if topicMatches(ex, topic) {
			return "", false
		}
	}
	for _, f := range r.cfg.Topics {
		if topicMatches(f, topic) {
			return f, true
		}
	}
	return "", false
}

// deviceName is what the wildcards of a filter matched, which is nearly always
// the name of the thing: "+/+/bridge/state" on "haus/shelly/bridge/state" gives
// "haus/shelly", "zigbee2mqtt/+" gives the sensor. A filter without wildcards
// names the thing itself.
func deviceName(filter, topic string) string {
	f := strings.Split(filter, "/")
	t := strings.Split(topic, "/")
	var parts []string
	for i, part := range f {
		if part == "#" {
			parts = append(parts, t[min(i, len(t)):]...)
			break
		}
		if part == "+" && i < len(t) {
			parts = append(parts, t[i])
		}
	}
	if len(parts) == 0 {
		return topic
	}
	return strings.Join(parts, "/")
}

// groupName is the display name of a whole filter: "wolf-cwl/#" -> "wolf-cwl".
func groupName(filter string) string {
	name := strings.TrimSuffix(strings.TrimSuffix(filter, "#"), "/")
	if name == "" {
		return filter
	}
	return name
}

// resolvedTitle is the headline of the recovery. Reusing the alert title there
// reads as the opposite of what happened ("Working again: x is offline").
func (r *Rule) resolvedTitle(device, topic, value string, labels map[string]string) string {
	tpl := r.cfg.ResolvedTitle
	if tpl == "" {
		tpl = "{device}: back to normal"
	}
	return r.render(tpl, device, topic, value, labels)
}

// title renders the rule's title template for one alert.
func (r *Rule) title(device, topic, value string, labels map[string]string) string {
	tpl := r.cfg.Title
	if tpl == "" {
		tpl = "{device}"
	}
	return r.render(tpl, device, topic, value, labels)
}

// render fills a title template. labels are the series labels of a promql
// alert ({mountpoint}, {namespace}, ...); the built-in placeholders win.
func (r *Rule) render(tpl, device, topic, value string, labels map[string]string) string {
	pairs := []string{
		"{device}", device,
		"{topic}", topic,
		"{value}", value,
		"{rule}", r.cfg.Name,
	}
	for k, v := range labels {
		pairs = append(pairs, "{"+k+"}", v)
	}
	return strings.NewReplacer(pairs...).Replace(tpl)
}

// matches evaluates the condition and returns the value it looked at, which
// ends up in the mail so the reader sees what was actually on the wire.
func (r *Rule) matches(payload []byte) (bool, string) {
	c := r.cfg.Condition
	if c == nil {
		return true, ""
	}

	value, ok := extractValue(payload, c.Field)
	if !ok {
		return false, ""
	}

	if c.Equals != nil && value != *c.Equals {
		return false, value
	}
	if c.NotEquals != nil && value == *c.NotEquals {
		return false, value
	}
	if r.regex != nil && !r.regex.MatchString(value) {
		return false, value
	}
	if c.LT != nil || c.GT != nil {
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return false, value
		}
		if c.LT != nil && !(n < *c.LT) {
			return false, value
		}
		if c.GT != nil && !(n > *c.GT) {
			return false, value
		}
	}
	return true, value
}

// extractValue returns the payload, or the JSON field at the dot path, as text.
func extractValue(payload []byte, field string) (string, bool) {
	if field == "" {
		return strings.TrimSpace(string(payload)), true
	}

	var doc any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return "", false
	}
	for _, key := range strings.Split(field, ".") {
		obj, ok := doc.(map[string]any)
		if !ok {
			return "", false
		}
		doc, ok = obj[key]
		if !ok {
			return "", false
		}
	}

	switch v := doc.(type) {
	case string:
		return v, true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	case nil:
		return "null", true
	default:
		b, _ := json.Marshal(v)
		return string(b), true
	}
}

// topicMatches implements MQTT filter matching ("+" one level, "#" the rest).
func topicMatches(filter, topic string) bool {
	f := strings.Split(filter, "/")
	t := strings.Split(topic, "/")
	for i, part := range f {
		if part == "#" {
			return true
		}
		if i >= len(t) {
			return false
		}
		if part != "+" && part != t[i] {
			return false
		}
	}
	return len(f) == len(t)
}

// filterCovers reports whether every topic matched by inner is also matched by
// outer, so subscribing to both would only deliver the same message twice.
func filterCovers(outer, inner string) bool {
	o := strings.Split(outer, "/")
	in := strings.Split(inner, "/")
	for i, part := range o {
		if part == "#" {
			return true
		}
		if i >= len(in) {
			return false
		}
		if in[i] == "#" {
			return false
		}
		if part != "+" && part != in[i] {
			return false
		}
	}
	return len(o) == len(in)
}

// isConcrete reports whether a filter names exactly one topic.
func isConcrete(filter string) bool {
	return !strings.ContainsAny(filter, "+#")
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour && int(d.Minutes())%60 == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute && int(d.Seconds())%60 == 0:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// summary is the one-line, human readable form of the rule for the UI and mails.
func (r *Rule) summary() string {
	cond := ""
	if c := r.cfg.Condition; c != nil {
		subject := "payload"
		if c.Field != "" {
			subject = c.Field
		}
		var parts []string
		if c.Equals != nil {
			parts = append(parts, fmt.Sprintf("%s = %q", subject, *c.Equals))
		}
		if c.NotEquals != nil {
			parts = append(parts, fmt.Sprintf("%s != %q", subject, *c.NotEquals))
		}
		if c.Regex != "" {
			parts = append(parts, fmt.Sprintf("%s ~ /%s/", subject, c.Regex))
		}
		if c.LT != nil {
			parts = append(parts, fmt.Sprintf("%s < %v", subject, *c.LT))
		}
		if c.GT != nil {
			parts = append(parts, fmt.Sprintf("%s > %v", subject, *c.GT))
		}
		cond = strings.Join(parts, " and ")
	}

	switch r.cfg.Type {
	case config.RuleState:
		if r.cfg.For > 0 {
			return cond + " for " + humanDuration(r.cfg.For.Std())
		}
		return cond
	case config.RuleCount:
		return fmt.Sprintf("%s, %d times within %s", cond, r.cfg.Count, humanDuration(r.cfg.Within.Std()))
	case config.RuleSilence:
		return "no message for " + humanDuration(r.cfg.For.Std())
	case config.RulePromQL:
		query := strings.Join(strings.Fields(r.cfg.Query), " ")
		if r.cfg.For > 0 {
			return query + " for " + humanDuration(r.cfg.For.Std())
		}
		return query
	}
	return cond
}
