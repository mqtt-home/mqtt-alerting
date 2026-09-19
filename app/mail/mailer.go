package mail

import (
	"errors"
	"fmt"
	"mime"
	"net/smtp"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mqtt-home/mqtt-mail/config"
	"github.com/philipparndt/go-logger"
)

// errDisabled marks a mail that was composed but deliberately not sent. It is
// not a failure: the events are dropped from the queue, just not counted as sent.
var errDisabled = errors.New("mail sending is disabled")

// SendFunc delivers one mail. Swapped out in tests.
type SendFunc func(cfg config.SMTPConfig, subject, body string) error

// Mailer collects events and turns them into as few mails as possible: events
// arriving within the batch window share one mail, and beyond the hourly
// budget mails are held back (and keep batching) rather than dropped. A broker
// restart or a flapping device must not be able to flood the inbox.
type Mailer struct {
	cfg  config.MailConfig
	send SendFunc

	mu          sync.Mutex
	queue       []Event
	firstQueued time.Time
	sentAt      []time.Time
	sent        int
	lastSentAt  *time.Time
	lastError   string
	lastErrorAt *time.Time

	onChange func()
}

func NewMailer(cfg config.MailConfig) *Mailer {
	return &Mailer{cfg: cfg, send: SendSMTP}
}

func (m *Mailer) OnChange(f func()) { m.onChange = f }

// Enqueue adds an event to the next mail.
func (m *Mailer) Enqueue(ev Event) {
	m.mu.Lock()
	if len(m.queue) == 0 {
		m.firstQueued = ev.At
	}
	m.queue = append(m.queue, ev)
	m.mu.Unlock()
}

// Flush sends the queued events if the batch window has passed and the hourly
// budget allows it.
func (m *Mailer) Flush(now time.Time) {
	m.mu.Lock()
	if len(m.queue) == 0 || now.Sub(m.firstQueued) < time.Duration(m.cfg.BatchSeconds)*time.Second {
		m.mu.Unlock()
		return
	}

	cutoff := now.Add(-time.Hour)
	recent := m.sentAt[:0]
	for _, t := range m.sentAt {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	m.sentAt = recent
	if len(m.sentAt) >= m.cfg.MaxMailsPerHour {
		m.mu.Unlock()
		return
	}

	events := m.queue
	m.queue = nil
	m.mu.Unlock()

	subject, body := compose(m.cfg.SubjectPrefix, events, now)
	err := m.deliver(subject, body)

	if errors.Is(err, errDisabled) {
		return
	}

	m.mu.Lock()
	if err != nil {
		msg := err.Error()
		m.lastError = msg
		m.lastErrorAt = &now
		// Put the events back in front: the next flush retries them together
		// with whatever arrived in the meantime.
		m.queue = append(events, m.queue...)
		m.firstQueued = now
	} else {
		m.sent++
		m.sentAt = append(m.sentAt, now)
		m.lastSentAt = &now
		m.lastError = ""
		m.lastErrorAt = nil
	}
	m.mu.Unlock()

	if m.onChange != nil {
		m.onChange()
	}
}

// SendTest bypasses the queue and the budget: it is how you find out whether
// the SMTP account works, so it must report the error instead of retrying.
func (m *Mailer) SendTest(now time.Time) error {
	host, _ := os.Hostname()
	body := fmt.Sprintf("This is a test mail from mqtt-mail.\n\nSent: %s\nHost: %s\n",
		now.Format(time.RFC1123), host)
	err := m.deliver(prefixed(m.cfg.SubjectPrefix, "Test mail"), body)

	m.mu.Lock()
	if err != nil {
		m.lastError = err.Error()
		m.lastErrorAt = &now
	} else {
		m.sent++
		m.sentAt = append(m.sentAt, now)
		m.lastSentAt = &now
		m.lastError = ""
		m.lastErrorAt = nil
	}
	m.mu.Unlock()

	if m.onChange != nil {
		m.onChange()
	}
	return err
}

func (m *Mailer) deliver(subject, body string) error {
	if !m.cfg.SMTP.Enabled {
		logger.Info("Mail disabled, not sending", "subject", subject)
		return errDisabled
	}
	if err := m.send(m.cfg.SMTP, subject, body); err != nil {
		logger.Error("Failed to send mail", "subject", subject, "error", err)
		return err
	}
	logger.Info("Mail sent", "subject", subject)
	return nil
}

func (m *Mailer) Stats() MailStats {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-time.Hour)
	lastHour := 0
	for _, t := range m.sentAt {
		if t.After(cutoff) {
			lastHour++
		}
	}
	return MailStats{
		Enabled:      m.cfg.SMTP.Enabled,
		Sent:         m.sent,
		Queued:       len(m.queue),
		LastSentAt:   m.lastSentAt,
		LastError:    m.lastError,
		LastErrorAt:  m.lastErrorAt,
		SentLastHour: lastHour,
	}
}

// Run flushes until stop is closed.
func (m *Mailer) Run(interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			m.Flush(now)
		case <-stop:
			return
		}
	}
}

func prefixed(prefix, subject string) string {
	if prefix == "" {
		return subject
	}
	return prefix + " " + subject
}

var kindLabel = map[string]string{
	EventFiring:   "ALERT",
	EventResolved: "RESOLVED",
	EventReminder: "STILL FIRING",
}

// compose renders a batch of events as one plain-text mail.
func compose(prefix string, events []Event, now time.Time) (string, string) {
	counts := map[string]int{}
	for _, ev := range events {
		counts[ev.Kind]++
	}

	var subject string
	if len(events) == 1 {
		ev := events[0]
		subject = fmt.Sprintf("%s %s: %s", kindLabel[ev.Kind], ev.Alert.Rule, ev.Alert.Topic)
	} else {
		var parts []string
		for _, kind := range []string{EventFiring, EventReminder, EventResolved} {
			if counts[kind] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", counts[kind], strings.ToLower(kindLabel[kind])))
			}
		}
		subject = fmt.Sprintf("%d alerts (%s)", len(events), strings.Join(parts, ", "))
	}

	var b strings.Builder
	for _, kind := range []string{EventFiring, EventReminder, EventResolved} {
		if counts[kind] == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s\n%s\n\n", kindLabel[kind], strings.Repeat("=", len(kindLabel[kind])))
		for _, ev := range events {
			if ev.Kind != kind {
				continue
			}
			a := ev.Alert
			fmt.Fprintf(&b, "%s\n", a.Topic)
			fmt.Fprintf(&b, "  rule:   %s", a.Rule)
			if a.Description != "" {
				fmt.Fprintf(&b, " - %s", a.Description)
			}
			b.WriteString("\n")
			if a.Value != "" {
				fmt.Fprintf(&b, "  value:  %s\n", a.Value)
			}
			if !a.Since.IsZero() {
				fmt.Fprintf(&b, "  since:  %s\n", a.Since.Local().Format("Mon 02 Jan 15:04:05"))
			}
			if ev.Duration != "" {
				fmt.Fprintf(&b, "  firing: %s\n", ev.Duration)
			}
			fmt.Fprintf(&b, "  at:     %s\n\n", ev.At.Local().Format("Mon 02 Jan 15:04:05"))
		}
	}
	fmt.Fprintf(&b, "-- \nmqtt-mail, %s\n", now.Local().Format(time.RFC1123))

	return prefixed(prefix, subject), b.String()
}

// SendSMTP delivers over SMTP with STARTTLS (net/smtp upgrades on its own when
// the server offers it, and refuses to send credentials in the clear).
func SendSMTP(cfg config.SMTPConfig, subject, body string) error {
	if cfg.Host == "" || cfg.From == "" || cfg.To == "" {
		return fmt.Errorf("smtp host, from and to are required")
	}

	var to []string
	for _, addr := range strings.Split(cfg.To, ",") {
		if addr = strings.TrimSpace(addr); addr != "" {
			to = append(to, addr)
		}
	}

	host, _ := os.Hostname()
	now := time.Now()
	headers := []string{
		"From: " + cfg.From,
		"To: " + strings.Join(to, ", "),
		"Subject: " + mime.QEncoding.Encode("utf-8", subject),
		"Date: " + now.Format(time.RFC1123Z),
		fmt.Sprintf("Message-ID: <%d.mqtt-mail@%s>", now.UnixNano(), host),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: 8bit",
		"Auto-Submitted: auto-generated",
	}
	msg := strings.Join(headers, "\r\n") + "\r\n\r\n" + strings.ReplaceAll(body, "\n", "\r\n")

	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	return smtp.SendMail(addr, auth, cfg.From, to, []byte(msg))
}
