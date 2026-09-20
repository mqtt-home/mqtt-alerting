package mail

import (
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mqtt-home/mqtt-alerting/config"
	"github.com/philipparndt/go-logger"
)

// errDisabled marks a mail that was composed but deliberately not sent. It is
// not a failure: the events are dropped from the queue, just not counted as sent.
var errDisabled = errors.New("mail sending is disabled")

// SendFunc delivers one mail. Swapped out in tests.
type SendFunc func(cfg config.SMTPConfig, mail Mail) error

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
	snapshot func() Status
}

func NewMailer(cfg config.MailConfig) *Mailer {
	return &Mailer{cfg: cfg, send: SendSMTP}
}

func (m *Mailer) OnChange(f func()) { m.onChange = f }

// SetSnapshot provides the live status for the overview at the end of each mail.
func (m *Mailer) SetSnapshot(f func() Status) { m.snapshot = f }

func (m *Mailer) render(events []Event, now time.Time, demo bool) Mail {
	opt := renderOptions{Prefix: m.cfg.SubjectPrefix, UIURL: m.cfg.UIURL, Now: now, Demo: demo}
	if m.snapshot != nil {
		status := m.snapshot()
		opt.Status = &status
	}
	mail := compose(events, opt)
	if m.cfg.PlainText {
		mail.HTML = ""
	}
	return mail
}

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

	err := m.deliver(m.render(events, now, false))

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
// the SMTP account works, so it must report the error instead of retrying. It
// carries one made-up alert of every kind, so it also shows what real mails
// look like before the first real one arrives.
func (m *Mailer) SendTest(now time.Time) error {
	err := m.deliver(m.render(demoEvents(now), now, true))

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

func (m *Mailer) deliver(mail Mail) error {
	if !m.cfg.SMTP.Enabled {
		logger.Info("Mail disabled, not sending", "subject", mail.Subject)
		return errDisabled
	}
	if err := m.send(m.cfg.SMTP, mail); err != nil {
		logger.Error("Failed to send mail", "subject", mail.Subject, "error", err)
		return err
	}
	logger.Info("Mail sent", "subject", mail.Subject)
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

// SendSMTP delivers over SMTP with STARTTLS (net/smtp upgrades on its own when
// the server offers it, and refuses to send credentials in the clear).
func SendSMTP(cfg config.SMTPConfig, mail Mail) error {
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
	contentHeaders, body := mimeBody(mail, fmt.Sprintf("mqtt-alerting-%d", now.UnixNano()))
	headers := append([]string{
		"From: " + cfg.From,
		"To: " + strings.Join(to, ", "),
		"Subject: " + mime.QEncoding.Encode("utf-8", mail.Subject),
		"Date: " + now.Format(time.RFC1123Z),
		fmt.Sprintf("Message-ID: <%d.mqtt-alerting@%s>", now.UnixNano(), host),
		"MIME-Version: 1.0",
		"Auto-Submitted: auto-generated",
	}, contentHeaders...)
	msg := strings.Join(headers, "\r\n") + "\r\n\r\n" + body

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	return sendWithTimeout(addr, cfg, to, []byte(msg))
}

// smtpTimeout bounds the whole conversation with the mail server.
const smtpTimeout = 45 * time.Second

// sendWithTimeout is smtp.SendMail with a deadline. SendMail has none, so a
// mail server that accepts the connection and then goes quiet would block the
// mailer forever — and with it every later alert.
func sendWithTimeout(addr string, cfg config.SMTPConfig, to []string, msg []byte) error {
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(smtpTimeout)); err != nil {
		return err
	}

	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return err
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return err
		}
	} else if cfg.Username != "" {
		// Same rule as smtp.SendMail: never send credentials in the clear.
		return fmt.Errorf("smtp server %s does not offer STARTTLS, refusing to authenticate", cfg.Host)
	}
	if cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(cfg.From); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
