package replay

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// errNoRawSegments means the logger predates /api/segments/{day}/raw.
var errNoRawSegments = errors.New("mqtt-logger has no raw segment download")

// Stats describes where the history came from.
type Stats struct {
	Mode       string // "segments" or "events"
	Requests   int
	Bytes      int64
	Scanned    int // messages read
	Delivered  int // messages that some rule watches
	Unreadable int // lines that did not parse, e.g. today's last, half-written one
}

// Stream calls fn for every recorded message on the filters between from and
// to, in the order they were received, without holding them all in memory.
//
// It downloads each day's segment file once (mqtt-logger >= v1.6.0) and falls
// back to the much slower /api/events for older loggers.
func (h *History) Stream(ctx context.Context, filters []string, from, to time.Time, fn func(Message)) (Stats, error) {
	stats := Stats{Mode: "segments"}
	var days []string
	err := errNoRawSegments
	if !h.QueryAPIOnly {
		days, err = h.segmentDays(ctx, from, to)
	}
	if err == nil {
		matched := map[string]bool{}
		for _, day := range days {
			err = h.streamDay(ctx, day, filters, from, to, matched, &stats, fn)
			if errors.Is(err, errNoRawSegments) {
				break
			}
			if err != nil {
				return stats, err
			}
		}
	}
	if err == nil {
		return stats, nil
	}
	if !errors.Is(err, errNoRawSegments) {
		return stats, err
	}

	// Old logger: page through the query API.
	msgs, err := h.Fetch(ctx, filters, from, to)
	stats = Stats{Mode: "events", Requests: h.Requests, Scanned: len(msgs), Delivered: len(msgs)}
	if err != nil {
		return stats, err
	}
	for _, m := range msgs {
		fn(m)
	}
	return stats, nil
}

// segmentDays lists the stored days that can hold messages of [from, to]. The
// logger names days in its own time zone, so one day of margin on each side;
// messages outside the window are dropped by timestamp.
func (h *History) segmentDays(ctx context.Context, from, to time.Time) ([]string, error) {
	body, _, err := h.get(ctx, "/api/segments")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var listing struct {
		Segments []struct {
			Day string `json:"day"`
		} `json:"segments"`
	}
	if err := json.NewDecoder(body).Decode(&listing); err != nil {
		return nil, errNoRawSegments
	}
	lo := from.AddDate(0, 0, -1).Format("2006-01-02")
	hi := to.AddDate(0, 0, 1).Format("2006-01-02")
	var days []string
	for _, s := range listing.Segments {
		if s.Day >= lo && s.Day <= hi {
			days = append(days, s.Day)
		}
	}
	sort.Strings(days)
	return days, nil
}

func (h *History) streamDay(ctx context.Context, day string, filters []string, from, to time.Time, matched map[string]bool, stats *Stats, fn func(Message)) error {
	body, contentType, err := h.get(ctx, "/api/segments/"+day+"/raw")
	if err != nil {
		return err
	}
	defer body.Close()
	stats.Requests++

	counted := &countingReader{r: body}
	var r io.Reader = counted
	switch {
	case strings.HasPrefix(contentType, "application/gzip"):
		gz, err := gzip.NewReader(counted)
		if err != nil {
			return fmt.Errorf("segment %s: %w", day, err)
		}
		defer gz.Close()
		r = gz
	case strings.HasPrefix(contentType, "application/x-ndjson"):
	default:
		// An older logger answers unknown paths with its web UI.
		return errNoRawSegments
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20) // map images and full appliance dumps are large
	for scanner.Scan() {
		stats.Scanned++
		var e loggerEvent
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			stats.Unreadable++
			continue
		}
		watched, known := matched[e.Topic]
		if !known {
			watched = firstMatch(filters, e.Topic) >= 0
			matched[e.Topic] = watched
		}
		if !watched {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil || t.Before(from) || t.After(to) {
			continue
		}
		stats.Delivered++
		fn(Message{Time: t, Topic: e.Topic, Payload: payload(e.Message)})
	}
	stats.Bytes += counted.n
	if err := scanner.Err(); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("segment %s: %w", day, err)
	}
	return nil
}

func (h *History) get(ctx context.Context, path string) (io.ReadCloser, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		resp.Body.Close()
		return nil, "", errNoRawSegments
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, "", fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
