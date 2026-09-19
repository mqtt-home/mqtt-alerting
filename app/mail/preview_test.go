package mail

import (
	"os"
	"testing"
)

// Writes sample mails to $MAIL_PREVIEW_DIR so the styling can be looked at in
// a browser:  MAIL_PREVIEW_DIR=/tmp/preview go test ./mail -run TestPreview
func TestPreview(t *testing.T) {
	dir := os.Getenv("MAIL_PREVIEW_DIR")
	if dir == "" {
		t.Skip("MAIL_PREVIEW_DIR not set")
	}

	fired := event(EventFiring, "haus/shelly/bridge/state", t0)
	fired.Alert.Title = "haus/shelly is offline"
	fired.Alert.Description = "a bridge reports offline"
	fired.Alert.Check = `payload = "offline" for 10m`
	resolved := event(EventResolved, "rules/bridge/state", t0)
	resolved.Alert.Title = "rules is offline"
	resolved.Duration = "1h 53m"

	opt := renderOptions{Prefix: "[smarthome]", UIURL: "https://mail.rnd7.de", Now: t0, Status: sampleStatus()}
	samples := map[string]Mail{
		"alert.html": compose([]Event{fired, resolved}, opt),
		"test.html":  compose(demoEvents(t0), renderOptions{Prefix: opt.Prefix, UIURL: opt.UIURL, Now: t0, Status: sampleStatus(), Demo: true}),
		"clear.html": compose([]Event{resolved}, renderOptions{Prefix: opt.Prefix, UIURL: opt.UIURL, Now: t0,
			Status: &Status{Rules: sampleStatus().Rules[1:2]}}),
	}
	for name, m := range samples {
		if err := os.WriteFile(dir+"/"+name, []byte(m.HTML), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+"/"+name+".txt", []byte(m.Subject+"\n\n"+m.Text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
