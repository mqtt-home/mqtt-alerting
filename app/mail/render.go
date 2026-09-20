package mail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"strings"
	"time"
)

// Mail is a rendered message: a subject, a plain text body and an HTML body
// that says the same thing. The text part is what a client without HTML (or a
// log line) gets, so it must never be an afterthought.
type Mail struct {
	Subject string
	Text    string
	HTML    string
}

var kindLabel = map[string]string{
	EventFiring:   "ALERT",
	EventResolved: "RESOLVED",
	EventReminder: "STILL FIRING",
}

var sectionTitle = map[string]string{
	EventFiring:   "What went wrong",
	EventReminder: "Still not fixed",
	EventResolved: "Working again",
}

var kindOrder = []string{EventFiring, EventReminder, EventResolved}

// Colours. Picked to stay readable when a mail client inverts them for dark
// mode, which is why nothing relies on a pure white or pure black.
const (
	colBad     = "#c62828"
	colBadBg   = "#fdecea"
	colWarn    = "#b26a00"
	colWarnBg  = "#fff4e0"
	colGood    = "#2e7d32"
	colGoodBg  = "#e8f5e9"
	colInk     = "#1f2933"
	colMuted   = "#66707c"
	colLine    = "#e3e7ec"
	colPage    = "#f2f4f7"
	colCard    = "#ffffff"
	fontSans   = "-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif"
	fontMono   = "ui-monospace,SFMono-Regular,Menlo,Consolas,monospace"
	timeLayout = "Mon 02 Jan 15:04"
)

func kindColours(kind string) (fg, bg string) {
	switch kind {
	case EventFiring:
		return colBad, colBadBg
	case EventReminder:
		return colWarn, colWarnBg
	default:
		return colGood, colGoodBg
	}
}

// renderOptions is everything compose needs besides the events.
type renderOptions struct {
	Prefix string
	UIURL  string
	Now    time.Time
	// Status is the live overview appended to every mail, so a mail about one
	// broken thing also says what is fine. Nil: no overview.
	Status *Status
	// Demo marks a test mail whose events are made up.
	Demo bool
}

func countKinds(events []Event) map[string]int {
	counts := map[string]int{}
	for _, ev := range events {
		counts[ev.Kind]++
	}
	return counts
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// openProblems are the firing alerts from the overview that this mail does not
// already talk about.
func openProblems(status *Status, events []Event) []Alert {
	if status == nil {
		return nil
	}
	mentioned := map[string]bool{}
	for _, ev := range events {
		mentioned[key(ev.Alert.Rule, ev.Alert.Topic)] = true
	}
	var open []Alert
	for _, a := range status.Alerts {
		if a.State == StateFiring && !mentioned[key(a.Rule, a.Topic)] {
			open = append(open, a)
		}
	}
	return open
}

func totalFiring(status *Status) int {
	if status == nil {
		return 0
	}
	n := 0
	for _, a := range status.Alerts {
		if a.State == StateFiring {
			n++
		}
	}
	return n
}

// headline is the one sentence that has to be right even if nothing else of
// the mail is read, plus the colour that goes with it.
func headline(events []Event, opt renderOptions) (text, kind string) {
	counts := countKinds(events)
	switch {
	case opt.Demo:
		return "Test mail — this is what an alert looks like", EventResolved
	case counts[EventFiring] > 0:
		return plural(counts[EventFiring], "problem needs", "problems need") + " attention", EventFiring
	case counts[EventReminder] > 0:
		return plural(counts[EventReminder], "problem is", "problems are") + " still not fixed", EventReminder
	}
	if open := totalFiring(opt.Status); open > 0 {
		return fmt.Sprintf("%s, %s", plural(counts[EventResolved], "thing works again", "things work again"),
			plural(open, "problem still open", "problems still open")), EventReminder
	}
	return plural(counts[EventResolved], "thing works again", "things work again") + " — all clear", EventResolved
}

func subjectOf(events []Event, opt renderOptions) string {
	if opt.Demo {
		return prefixed(opt.Prefix, "Test mail")
	}
	counts := countKinds(events)
	if len(events) == 1 {
		ev := events[0]
		return prefixed(opt.Prefix, fmt.Sprintf("%s: %s", kindLabel[ev.Kind], ev.Alert.Title))
	}
	var parts []string
	for _, kind := range kindOrder {
		if counts[kind] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[kind], strings.ToLower(kindLabel[kind])))
		}
	}
	return prefixed(opt.Prefix, fmt.Sprintf("%d alerts (%s)", len(events), strings.Join(parts, ", ")))
}

// compose renders a batch of events as one mail.
func compose(events []Event, opt renderOptions) Mail {
	return Mail{
		Subject: subjectOf(events, opt),
		Text:    composeText(events, opt),
		HTML:    composeHTML(events, opt),
	}
}

// --- plain text ---

func composeText(events []Event, opt renderOptions) string {
	var b strings.Builder
	counts := countKinds(events)

	head, _ := headline(events, opt)
	fmt.Fprintf(&b, "%s\n\n", head)
	if opt.Demo {
		b.WriteString("The examples below are made up. The overview at the end is real.\n\n")
	}

	for _, kind := range kindOrder {
		if counts[kind] == 0 {
			continue
		}
		title := strings.ToUpper(sectionTitle[kind])
		fmt.Fprintf(&b, "%s\n%s\n\n", title, strings.Repeat("=", len(title)))
		for _, ev := range events {
			if ev.Kind != kind {
				continue
			}
			a := ev.Alert
			fmt.Fprintf(&b, "%s\n", a.Title)
			if a.Description != "" {
				fmt.Fprintf(&b, "  what:   %s\n", a.Description)
			}
			fmt.Fprintf(&b, "  topic:  %s\n", a.Topic)
			if a.Value != "" && ev.Kind == EventResolved {
				fmt.Fprintf(&b, "  now:    %s\n", a.Value)
			} else if a.Value != "" {
				fmt.Fprintf(&b, "  value:  %s\n", a.Value)
			}
			if a.Check != "" {
				fmt.Fprintf(&b, "  check:  %s\n", a.Check)
			}
			if !a.Since.IsZero() {
				fmt.Fprintf(&b, "  since:  %s\n", a.Since.Local().Format(timeLayout))
			}
			switch {
			case ev.Kind == EventResolved && ev.Duration != "":
				fmt.Fprintf(&b, "  down:   %s\n", ev.Duration)
			case ev.Duration != "":
				fmt.Fprintf(&b, "  open:   %s\n", ev.Duration)
			}
			b.WriteString("\n")
		}
	}

	if open := openProblems(opt.Status, events); len(open) > 0 {
		b.WriteString("OTHER OPEN PROBLEMS\n===================\n\n")
		for _, a := range open {
			fmt.Fprintf(&b, "%s  (%s)\n", a.Title, a.Topic)
		}
		b.WriteString("\n")
	}

	if opt.Status != nil {
		b.WriteString("OVERVIEW\n========\n\n")
		for _, r := range opt.Status.Rules {
			state := "ok"
			if r.Firing > 0 {
				state = fmt.Sprintf("%d FIRING", r.Firing)
			}
			fmt.Fprintf(&b, "  %-26s %4d watched   %s\n", r.Name, r.Watching, state)
		}
		b.WriteString("\n")
	}

	if opt.UIURL != "" {
		fmt.Fprintf(&b, "Dashboard: %s\n", opt.UIURL)
	}
	fmt.Fprintf(&b, "-- \nmqtt-alerting, %s\n", opt.Now.Local().Format(time.RFC1123))
	return b.String()
}

// --- HTML ---
//
// Mail clients are browsers from 2003: no external CSS, no flexbox, tables for
// layout and every style inline. Everything user controlled goes through esc().

func esc(s string) string { return html.EscapeString(s) }

func pill(text, fg, bg string) string {
	return fmt.Sprintf(`<span style="display:inline-block;padding:2px 9px;border-radius:10px;background:%s;color:%s;font-size:12px;font-weight:600;white-space:nowrap;">%s</span>`,
		bg, fg, esc(text))
}

func detailRow(label, value string, mono bool) string {
	if value == "" {
		return ""
	}
	font := fontSans
	if mono {
		font = fontMono
	}
	return fmt.Sprintf(`<tr><td style="padding:2px 12px 2px 0;color:%s;font-size:13px;vertical-align:top;white-space:nowrap;">%s</td>`+
		`<td style="padding:2px 0;color:%s;font-size:13px;font-family:%s;word-break:break-all;">%s</td></tr>`,
		colMuted, esc(label), colInk, font, esc(value))
}

func eventCard(ev Event) string {
	fg, bg := kindColours(ev.Kind)
	a := ev.Alert

	var rows strings.Builder
	rows.WriteString(detailRow("What", a.Description, false))
	rows.WriteString(detailRow("Topic", a.Topic, true))
	valueLabel := "Value"
	if ev.Kind == EventResolved {
		valueLabel = "Now"
	}
	rows.WriteString(detailRow(valueLabel, a.Value, true))
	rows.WriteString(detailRow("Check", a.Check, false))
	if !a.Since.IsZero() {
		rows.WriteString(detailRow("Since", a.Since.Local().Format(timeLayout), false))
	}
	switch {
	case ev.Kind == EventResolved && ev.Duration != "":
		rows.WriteString(detailRow("Was down", ev.Duration, false))
	case ev.Duration != "":
		rows.WriteString(detailRow("Open for", ev.Duration, false))
	}

	return fmt.Sprintf(`<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" style="margin:0 0 10px 0;border-collapse:separate;">`+
		`<tr><td style="background:%s;border-left:4px solid %s;border-radius:6px;padding:12px 14px;">`+
		`<div style="font-size:16px;font-weight:600;color:%s;margin:0 0 6px 0;">%s</div>`+
		`<table role="presentation" cellpadding="0" cellspacing="0">%s</table>`+
		`</td></tr></table>`,
		bg, fg, colInk, esc(a.Title), rows.String())
}

func sectionHeading(text, colour string) string {
	return fmt.Sprintf(`<div style="font-size:12px;font-weight:700;letter-spacing:.06em;text-transform:uppercase;color:%s;margin:22px 0 8px 0;">%s</div>`,
		colour, esc(text))
}

func composeHTML(events []Event, opt renderOptions) string {
	var b strings.Builder
	counts := countKinds(events)
	head, headKind := headline(events, opt)
	headFg, _ := kindColours(headKind)

	var sub []string
	if opt.Status != nil {
		watched := 0
		for _, r := range opt.Status.Rules {
			watched += r.Watching
		}
		sub = append(sub, plural(len(opt.Status.Rules), "rule", "rules"), plural(watched, "thing watched", "things watched"))
		if open := totalFiring(opt.Status); open > 0 {
			sub = append(sub, plural(open, "open problem", "open problems"))
		} else {
			sub = append(sub, "no open problems")
		}
	}

	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<meta name="color-scheme" content="light"><meta name="supported-color-schemes" content="light">` +
		`<title>` + esc(subjectOf(events, opt)) + `</title></head>`)
	fmt.Fprintf(&b, `<body style="margin:0;padding:0;background:%s;">`, colPage)
	// Preheader: what the inbox list shows next to the subject.
	fmt.Fprintf(&b, `<div style="display:none;max-height:0;overflow:hidden;opacity:0;">%s</div>`, esc(head))
	fmt.Fprintf(&b, `<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" style="background:%s;"><tr><td align="center" style="padding:20px 10px;">`, colPage)
	fmt.Fprintf(&b, `<table role="presentation" width="600" cellpadding="0" cellspacing="0" style="width:100%%;max-width:600px;font-family:%s;">`, fontSans)

	// Banner
	fmt.Fprintf(&b, `<tr><td style="background:%s;border-radius:8px 8px 0 0;padding:20px 22px;">`+
		`<div style="font-size:12px;font-weight:600;letter-spacing:.08em;text-transform:uppercase;color:#ffffff;opacity:.85;">%s</div>`+
		`<div style="font-size:21px;line-height:1.3;font-weight:700;color:#ffffff;margin-top:4px;">%s</div>`+
		`<div style="font-size:13px;color:#ffffff;opacity:.9;margin-top:6px;">%s</div>`+
		`</td></tr>`,
		headFg, esc(strings.TrimSpace(strings.Trim(opt.Prefix, "[]() ")+" alerts")), esc(head), esc(strings.Join(sub, " · ")))

	fmt.Fprintf(&b, `<tr><td style="background:%s;border-radius:0 0 8px 8px;padding:4px 22px 22px 22px;">`, colCard)

	if opt.Demo {
		fmt.Fprintf(&b, `<div style="margin:18px 0 0 0;padding:10px 12px;border:1px dashed %s;border-radius:6px;font-size:13px;color:%s;">`+
			`The three examples below are <b>made up</b> to show the styles. The overview at the end is <b>real</b>, `+
			`and so is the fact that this mail arrived: SMTP works.</div>`, colLine, colMuted)
	}

	for _, kind := range kindOrder {
		if counts[kind] == 0 {
			continue
		}
		fg, _ := kindColours(kind)
		b.WriteString(sectionHeading(fmt.Sprintf("%s (%d)", sectionTitle[kind], counts[kind]), fg))
		for _, ev := range events {
			if ev.Kind == kind {
				b.WriteString(eventCard(ev))
			}
		}
	}

	if open := openProblems(opt.Status, events); len(open) > 0 {
		b.WriteString(sectionHeading(fmt.Sprintf("Other open problems (%d)", len(open)), colWarn))
		fmt.Fprintf(&b, `<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" style="border:1px solid %s;border-radius:6px;">`, colLine)
		for i, a := range open {
			border := ""
			if i > 0 {
				border = "border-top:1px solid " + colLine + ";"
			}
			fmt.Fprintf(&b, `<tr><td style="%spadding:8px 12px;font-size:14px;color:%s;">%s<div style="font-family:%s;font-size:12px;color:%s;word-break:break-all;">%s</div></td>`+
				`<td align="right" style="%spadding:8px 12px;font-size:12px;color:%s;white-space:nowrap;vertical-align:top;">since %s</td></tr>`,
				border, colInk, esc(a.Title), fontMono, colMuted, esc(a.Topic),
				border, colMuted, esc(a.Since.Local().Format(timeLayout)))
		}
		b.WriteString(`</table>`)
	}

	if opt.Status != nil && len(opt.Status.Rules) > 0 {
		b.WriteString(sectionHeading("Overview — what is being watched", colMuted))
		fmt.Fprintf(&b, `<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" style="border:1px solid %s;border-radius:6px;">`, colLine)
		for i, r := range opt.Status.Rules {
			border := ""
			if i > 0 {
				border = "border-top:1px solid " + colLine + ";"
			}
			state := pill("OK", colGood, colGoodBg)
			switch {
			case r.Firing > 0:
				state = pill(fmt.Sprintf("%d firing", r.Firing), colBad, colBadBg)
			case r.Pending > 0:
				state = pill(fmt.Sprintf("%d pending", r.Pending), colWarn, colWarnBg)
			case r.Watching == 0:
				state = pill("nothing seen yet", colMuted, colPage)
			}
			label := r.Description
			if label == "" {
				label = r.Summary
			}
			fmt.Fprintf(&b, `<tr><td style="%spadding:8px 12px;"><div style="font-size:14px;font-weight:600;color:%s;">%s</div>`+
				`<div style="font-size:12px;color:%s;">%s</div></td>`+
				`<td align="right" style="%spadding:8px 6px;font-size:12px;color:%s;white-space:nowrap;">%d watched</td>`+
				`<td align="right" style="%spadding:8px 12px;white-space:nowrap;">%s</td></tr>`,
				border, colInk, esc(r.Name), colMuted, esc(label),
				border, colMuted, r.Watching,
				border, state)
		}
		b.WriteString(`</table>`)
	}

	if opt.UIURL != "" {
		fmt.Fprintf(&b, `<div style="margin:22px 0 0 0;"><a href="%s" style="display:inline-block;background:%s;color:#ffffff;text-decoration:none;font-size:14px;font-weight:600;padding:9px 16px;border-radius:6px;">Open dashboard</a></div>`,
			esc(opt.UIURL), colInk)
	}

	b.WriteString(`</td></tr>`)
	fmt.Fprintf(&b, `<tr><td style="padding:12px 22px;font-size:12px;color:%s;">mqtt-alerting · %s</td></tr>`,
		colMuted, esc(opt.Now.Local().Format(time.RFC1123)))
	b.WriteString(`</table></td></tr></table></body></html>`)
	return b.String()
}

// demoEvents are the made-up alerts of the test mail: one of each kind, so the
// test shows every style the real mails use.
func demoEvents(now time.Time) []Event {
	firedAt := now.Add(-12 * time.Minute)
	longAgo := now.Add(-26 * time.Hour)
	return []Event{
		{Kind: EventFiring, At: now, Alert: Alert{
			Rule: "bridge-offline", Title: "example/heating is offline", Description: "a bridge reports offline",
			Check: `payload = "offline" for 10m`, Type: "state", Topic: "example/heating/bridge/state",
			State: StateFiring, Value: "offline", Since: firedAt, FiredAt: &now,
		}},
		{Kind: EventReminder, At: now, Duration: "1d 2h", Alert: Alert{
			Rule: "battery-low", Title: "example_window_sensor: battery at 7 %", Description: "a battery needs replacing",
			Check: "battery < 15 for 1h", Type: "state", Topic: "zigbee2mqtt/example_window_sensor",
			State: StateFiring, Value: "7", Since: longAgo, FiredAt: &longAgo,
		}},
		{Kind: EventResolved, At: now, Duration: "47m", Alert: Alert{
			Rule: "data-silent", Title: "example/weather sends data again", Description: "a service stopped publishing",
			Check: "no message for 30m", Type: "silence", Topic: "example/weather/#",
			State: StatePending, Since: now.Add(-47 * time.Minute),
		}},
	}
}

// --- MIME ---

func base64Lines(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var b bytes.Buffer
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteString("\r\n")
	return b.String()
}

// mimeBody returns the content headers and the encoded body. Base64 keeps
// every line under the SMTP limit no matter how long a topic or a payload is.
func mimeBody(m Mail, boundary string) (headers []string, body string) {
	if m.HTML == "" {
		return []string{
			"Content-Type: text/plain; charset=utf-8",
			"Content-Transfer-Encoding: base64",
		}, base64Lines(m.Text)
	}
	var b strings.Builder
	for _, part := range []struct{ typ, content string }{
		{"text/plain", m.Text},
		{"text/html", m.HTML}, // last part wins: clients prefer it when they can
	} {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n%s",
			boundary, part.typ, base64Lines(part.content))
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []string{fmt.Sprintf("Content-Type: multipart/alternative; boundary=%q", boundary)}, b.String()
}
