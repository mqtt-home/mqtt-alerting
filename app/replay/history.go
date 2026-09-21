// Package replay runs recorded MQTT history through the rule engine, offline:
// it never connects to a broker and never sends mail. It answers "what would
// this rule have done over the last two weeks?" before the rule goes live.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Message is one recorded MQTT message.
type Message struct {
	Time    time.Time
	Topic   string
	Payload []byte
}

// History reads recorded messages from an mqtt-logger instance
// (https://github.com/philipparndt/mqtt-logger), whose /api/events returns at
// most PageSize events per request and says so with "truncated".
//
// It is deliberately slow. mqtt-logger is the house's only record and runs with
// little memory: four parallel requests of 20000 events each got it OOM-killed
// on 2026-09-21. One request at a time with small pages leaves it untouched.
type History struct {
	BaseURL string
	Client  *http.Client
	// Parallel bounds concurrent requests. Defaults to 1.
	Parallel int
	// PageSize is the limit per request. Defaults to 5000.
	PageSize int
	// RetryWait is how long to wait before retrying a 5xx. Defaults to 15s.
	RetryWait time.Duration
	// QueryAPIOnly skips the day-file download and pages through /api/events.
	QueryAPIOnly bool
	// Requests counts the HTTP requests made, for the summary.
	Requests int

	mu sync.Mutex
}

type loggerEvent struct {
	Timestamp string          `json:"timestamp"`
	Topic     string          `json:"topic"`
	Message   json.RawMessage `json:"message"`
}

// payload restores the original bytes. mqtt-logger embeds a JSON payload as
// JSON and stores everything else as a JSON string. A payload that was itself
// a quoted JSON string comes back unquoted - indistinguishable in the store.
func payload(raw json.RawMessage) []byte {
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return []byte(s)
		}
	}
	return []byte(raw)
}

// Fetch returns every message on the filters between from and to, in the
// order they were received. A topic matched by several filters is taken from
// the first one only: the broker delivers a message once per client, not once
// per matching subscription.
func (h *History) Fetch(ctx context.Context, filters []string, from, to time.Time) ([]Message, error) {
	if h.Parallel <= 0 {
		h.Parallel = 1
	}
	if h.PageSize <= 0 {
		h.PageSize = 5000
	}
	if h.RetryWait <= 0 {
		h.RetryWait = 15 * time.Second
	}
	type job struct {
		filter   int
		from, to time.Time
	}
	var jobs []job
	for i := range filters {
		// Six-hour windows: most filters stay under one page, busy ones are split.
		for a := from; a.Before(to); a = a.Add(6 * time.Hour) {
			b := a.Add(6*time.Hour - time.Second)
			if b.After(to) {
				b = to
			}
			jobs = append(jobs, job{i, a, b})
		}
	}

	results := make([][]Message, len(jobs))
	errs := make(chan error, len(jobs))
	sem := make(chan struct{}, h.Parallel)
	var wg sync.WaitGroup
	for n, j := range jobs {
		wg.Add(1)
		go func(n int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			msgs, err := h.window(ctx, filters[j.filter], j.from, j.to)
			if err != nil {
				errs <- err
				return
			}
			var own []Message
			for _, m := range msgs {
				if firstMatch(filters, m.Topic) == j.filter {
					own = append(own, m)
				}
			}
			results[n] = own
		}(n, j)
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return nil, err
	}

	var all []Message
	for _, r := range results {
		all = append(all, r...)
	}
	// Timestamps have second resolution; a stable sort keeps the receive order
	// within a second, which matters for "offline" then "online" in one second.
	sort.SliceStable(all, func(i, j int) bool { return all[i].Time.Before(all[j].Time) })
	return all, nil
}

// window fetches one time window, halving it while the store says there is
// more than one response can hold.
func (h *History) window(ctx context.Context, filter string, from, to time.Time) ([]Message, error) {
	msgs, truncated, err := h.request(ctx, filter, from, to)
	if err != nil {
		return nil, err
	}
	if !truncated {
		return msgs, nil
	}
	if to.Sub(from) < 2*time.Second {
		return nil, fmt.Errorf("%s: more than %d messages within %s at %s", filter, h.PageSize, to.Sub(from)+time.Second, from.Format(time.RFC3339))
	}
	// Both bounds are inclusive and second-aligned, so [from, mid-1s] and
	// [mid, to] neither overlap nor leave a gap.
	mid := from.Add(to.Sub(from) / 2).Truncate(time.Second)
	left, err := h.window(ctx, filter, from, mid.Add(-time.Second))
	if err != nil {
		return nil, err
	}
	right, err := h.window(ctx, filter, mid, to)
	if err != nil {
		return nil, err
	}
	return append(left, right...), nil
}

// request asks once, and once more after RetryWait if the answer is a 5xx:
// the logger restarting or a proxy timing out. A second failure ends the
// replay instead of hammering a service that is struggling.
func (h *History) request(ctx context.Context, filter string, from, to time.Time) ([]Message, bool, error) {
	msgs, truncated, err := h.requestOnce(ctx, filter, from, to)
	var serverErr *serverError
	if errors.As(err, &serverErr) {
		select {
		case <-time.After(h.RetryWait):
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
		return h.requestOnce(ctx, filter, from, to)
	}
	return msgs, truncated, err
}

type serverError struct{ msg string }

func (e *serverError) Error() string { return e.msg }

func (h *History) requestOnce(ctx context.Context, filter string, from, to time.Time) ([]Message, bool, error) {
	q := url.Values{}
	q.Set("topic", filter)
	q.Set("from", from.UTC().Format(time.RFC3339))
	q.Set("to", to.UTC().Format(time.RFC3339))
	q.Set("limit", fmt.Sprint(h.PageSize))
	endpoint := strings.TrimRight(h.BaseURL, "/") + "/api/events?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	h.mu.Lock()
	h.Requests++
	h.mu.Unlock()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode >= 500 {
		return nil, false, &serverError{fmt.Sprintf("%s: HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("%s: HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Events    []loggerEvent `json:"events"`
		Truncated bool          `json:"truncated"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false, fmt.Errorf("%s: not an mqtt-logger answer: %w", endpoint, err)
	}
	msgs := make([]Message, 0, len(parsed.Events))
	for _, e := range parsed.Events {
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			continue
		}
		msgs = append(msgs, Message{Time: t, Topic: e.Topic, Payload: payload(e.Message)})
	}
	return msgs, parsed.Truncated, nil
}

func firstMatch(filters []string, topic string) int {
	for i, f := range filters {
		if matches(f, topic) {
			return i
		}
	}
	return -1
}

// matches is MQTT filter matching ("+" one level, "#" the rest).
func matches(filter, topic string) bool {
	f := strings.Split(filter, "/")
	t := strings.Split(topic, "/")
	for i, part := range f {
		if part == "#" {
			return true
		}
		if i >= len(t) || (part != "+" && part != t[i]) {
			return false
		}
	}
	return len(f) == len(t)
}
