package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mqtt-home/mqtt-alerting/config"
	"github.com/mqtt-home/mqtt-alerting/mail"
	"github.com/mqtt-home/mqtt-alerting/replay"
	"github.com/philipparndt/go-logger"
)

const replayUsage = `Usage: mqtt-alerting --replay <config.json> --source <mqtt-logger URL> [flags]

Runs recorded MQTT history through the rules and prints what they would have
done. Offline: no broker connection, no mail.

`

// replayCommand implements `mqtt-alerting --replay`.
func replayCommand(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), replayUsage)
		fs.PrintDefaults()
	}
	source := fs.String("source", os.Getenv("MQTT_ALERTING_HISTORY"), "mqtt-logger base URL, e.g. https://log.example.com (env MQTT_ALERTING_HISTORY)")
	fromFlag := fs.String("from", "-7d", "start: RFC3339, YYYY-MM-DD, or relative like -14d / -36h")
	toFlag := fs.String("to", "now", "end, same formats")
	only := fs.String("rules", "", "comma separated rule names to replay (default: all)")
	quiet := fs.Bool("summary", false, "only print the per-rule summary, not every alert")
	eventsAPI := fs.Bool("events-api", false, "read through the slow query API instead of day files (to cross-check, or for mqtt-logger < v1.6.0)")

	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fs.Usage()
		return 2
	}
	configFile := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *source == "" {
		fmt.Fprintln(os.Stderr, "--source is required: the URL of the mqtt-logger that recorded the history")
		return 2
	}

	now := time.Now()
	from, err := parseWhen(*fromFlag, now, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "--from:", err)
		return 2
	}
	to, err := parseWhen(*toFlag, now, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "--to:", err)
		return 2
	}
	if !to.After(from) {
		fmt.Fprintln(os.Stderr, "--to must be after --from")
		return 2
	}

	logger.SetLevel("warn") // the engine logs every alert; the report says it better
	cfg, err := config.LoadConfig(configFile)
	if err != nil {
		return 1
	}

	var names []string
	if *only != "" {
		for _, n := range strings.Split(*only, ",") {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
	}
	rules, skipped, unknown := replay.Split(cfg.Mail.Rules, names)
	if len(unknown) > 0 {
		fmt.Fprintf(os.Stderr, "unknown rule(s): %s\n", strings.Join(unknown, ", "))
		return 2
	}
	if len(rules) == 0 {
		fmt.Fprintln(os.Stderr, "nothing to replay (promql rules cannot be replayed)")
		return 2
	}

	// Validates the rules and yields the subscriptions the service would make.
	probe, err := mail.NewEngine(rules, from)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid rules:", err)
		return 1
	}
	filters := probe.Subscriptions()

	fmt.Printf("Replaying %s .. %s (%s), %d rule(s)\n", from.Local().Format("2006-01-02 15:04"), to.Local().Format("2006-01-02 15:04"), human(to.Sub(from)), len(rules))
	if len(skipped) > 0 {
		fmt.Printf("Skipped, promql rules need Prometheus history: %s\n", strings.Join(skipped, ", "))
	}

	history := &replay.History{BaseURL: *source, Client: &http.Client{Timeout: 10 * time.Minute}, QueryAPIOnly: *eventsAPI}
	started := time.Now()
	var stats replay.Stats
	result, err := replay.Run(rules, func(fn func(replay.Message)) error {
		var err error
		stats, err = history.Stream(context.Background(), filters, from, to, fn)
		return err
	}, from, to)
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		return 1
	}
	origin := fmt.Sprintf("%d day file(s), %.1f MB", stats.Requests, float64(stats.Bytes)/1e6)
	if stats.Mode == "events" {
		origin = fmt.Sprintf("%d query request(s)", stats.Requests)
		if !*eventsAPI {
			origin += " - this mqtt-logger has no day-file download; v1.6.0 or later makes replays about 100x faster"
		}
	}
	fmt.Printf("Read %d messages, %d watched by the rules, from %s in %s\n", stats.Scanned, stats.Delivered, origin, time.Since(started).Round(100*time.Millisecond))
	if stats.Unreadable > 1 {
		fmt.Printf("%d unreadable lines skipped\n", stats.Unreadable)
	}

	if !*quiet {
		fmt.Println()
		fmt.Println("TIMELINE")
		if len(result.Events) == 0 {
			fmt.Println("  nothing would have fired")
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, ev := range result.Events {
			kind := map[string]string{mail.EventFiring: "ALERT", mail.EventReminder: "REMINDER", mail.EventResolved: "RESOLVED"}[ev.Kind]
			extra := ""
			if ev.Kind == mail.EventResolved && ev.Duration != "" {
				extra = "after " + ev.Duration
			} else if ev.Alert.Value != "" {
				extra = "value " + ev.Alert.Value
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", ev.At.Local().Format("Mon 01-02 15:04"), kind, ev.Alert.Rule, ev.Alert.Title, extra)
		}
		tw.Flush()
	}

	fmt.Println()
	fmt.Println("SUMMARY")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  rule\twatched\talerts\treminders\tlongest\ttotal firing")
	var blind []string
	for _, s := range result.Rules {
		longest, total := "-", "-"
		if s.Alerts > 0 {
			longest, total = human(s.Longest), human(s.Total)
		}
		if s.StillFiring > 0 {
			total += fmt.Sprintf(" (%d still firing)", s.StillFiring)
		}
		fmt.Fprintf(tw, "  %s\t%7d\t%6d\t%9d\t%7s\t%s\n", s.Name, s.Watched, s.Alerts, s.Reminders, longest, total)
		if s.Watched == 0 {
			blind = append(blind, s.Name)
		}
	}
	tw.Flush()
	if len(blind) > 0 {
		fmt.Printf("\nNever saw a single matching topic: %s\n", strings.Join(blind, ", "))
		fmt.Println("Check their topics - or the recording does not go back that far.")
	}
	fmt.Println("\nState is rebuilt from --from on: a condition that already held before it counts from --from.")
	return 0
}

var relative = regexp.MustCompile(`^-(\d+)([dhm])$`)

// parseWhen accepts RFC3339, YYYY-MM-DD, "now" and -Nd / -Nh / -Nm.
func parseWhen(s string, now time.Time, endOfDay bool) (time.Time, error) {
	if s == "now" {
		return now.Truncate(time.Second), nil
	}
	if m := relative.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"d": 24 * time.Hour, "h": time.Hour, "m": time.Minute}[m[2]]
		return now.Add(-time.Duration(n) * unit).Truncate(time.Second), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		if endOfDay {
			return t.Add(24*time.Hour - time.Second), nil
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not RFC3339, YYYY-MM-DD, now or -Nd/-Nh/-Nm", s)
}

// human truncates like the engine's durations do, so summary and timeline agree.
func human(d time.Duration) string {
	d = d.Round(time.Second).Truncate(time.Minute)
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}
