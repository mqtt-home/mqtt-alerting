# mqtt-mail

Watches MQTT topics with rules from its config and sends an email when one of
them says something is wrong. It exists because a bridge can look perfectly
healthy — running, publishing, answering pings — while the one thing it is for
has been broken for weeks.

## Rules

A rule is evaluated separately for every concrete topic its filters match, so a
single wildcard rule covers every service that follows the same convention.

| Type | Fires when | Needs |
|---|---|---|
| `state` | the condition holds continuously for `for` | `condition`, `for` (optional) |
| `count` | the condition matched `count` times within `within`; resolves after a quiet window | `condition`, `count`, `within` |
| `silence` | no message arrived for `for` | `for` |
| `silence` + `"group": true` | nothing at all arrived under a filter (`wolf-cwl/#`) — one alert per filter instead of one per topic | `for` |

```json
{
  "name": "bridge-offline",
  "description": "a bridge reports offline",
  "title": "{device} bridge is offline",
  "resolved_title": "{device} bridge is back online",
  "type": "state",
  "topics": ["+/bridge/state", "+/+/bridge/state"],
  "exclude": ["test/#"],
  "condition": { "equals": "offline" },
  "for": "10m",
  "repeat": "24h",
  "recovery": true
}
```

- `topics` / `exclude` — MQTT filters, `+` and `#` allowed.
- `title` / `resolved_title` — the headline of the alert and of its recovery.
  Placeholders: `{device}` (what the filter's wildcards matched: `+/+/bridge/state`
  on `haus/shelly/bridge/state` gives `haus/shelly`; for a group the filter
  without `/#`), `{topic}`, `{value}`, `{rule}`.
- `condition` — tests the raw payload, or with `field` a dot path into a JSON
  payload (`"field": "battery"`). Operators: `equals`, `not_equals`, `regex`,
  `lt`, `gt`; all that are set must hold.
- `for`, `within`, `repeat` — Go durations (`"90s"`, `"10m"`, `"24h"`).
- `repeat` — re-send a still-firing alert at this interval. Omit for one mail.
- `recovery` — send a mail when the alert resolves. Defaults to `true`.

A `silence` rule on a concrete topic starts its clock at startup, so a topic
that never says anything is caught too. With a wildcard filter a topic is
tracked from its first message on.

Rules that do not compile stop the service at startup rather than silently
watching nothing. Check a config before rolling it out:

```bash
mqtt-mail --check config.json
```

## Mail

Mails are HTML with a plain-text alternative (`"plain_text": true` for text
only). Each one leads with a single sentence that is right even if nothing else
is read, then lists what went wrong, what is still not fixed and what works
again, other problems that are still open, and an overview of every rule with
what it watches and whether it is fine — so a mail about one broken thing also
says what works. `ui_url` adds a link to the dashboard.

To look at the styling without sending anything:

```bash
cd app && MAIL_PREVIEW_DIR=/tmp/preview go test ./mail -run TestPreview   # writes *.html
```

The test mail (button in the UI, or `{"action": "test"}`) carries one made-up
alert of every kind plus the real overview.

Alerts are collected for `batch_seconds` (default 30) and sent as one mail.
Beyond `max_mails_per_hour` (default 12) mails are held back and keep batching —
never dropped. A failed delivery is retried with the next flush and shown in the
status. With `smtp.enabled: false` everything runs but mails are only logged,
which is the way to try out new rules.

## MQTT topics

| Topic | Direction | Payload |
|---|---|---|
| `home/mail/status` | published, retained | rules, current alerts, history, mail statistics |
| `home/mail/availability` | published, retained | `online` \| `offline` |
| `home/mail/set` | subscribed | `{"action": "test"}` sends a test mail |

The web UI shows the same status live and has a "Send test mail" button that
reports the SMTP error verbatim.

## Quick start

### Docker

```bash
docker run -d \
  -v /path/to/config:/var/lib/mqtt-mail \
  -p 8080:8080 \
  pharndt/mqtt-mail:latest
```

### From source

```bash
cd app
make dev          # build frontend + backend, run with production/config/config.json
make dev-frontend # vite dev server on :5173 against the backend on :8080
make test
```

## Configuration

See `production/config/config.example.json`. `${VAR}` placeholders are replaced
from the environment at startup, so secrets stay out of the config file.

```json
{
  "mqtt": {
    "url": "tcp://10.10.1.3:1883",
    "topic": "home/mail",
    "qos": 2,
    "retain": true
  },
  "mail": {
    "username": "${MAIL_USER}",
    "password": "${MAIL_PASSWORD}",
    "polling_interval": 30
  },
  "web": { "enabled": true, "port": 8080 },
  "loglevel": "info"
}
```

## Release

Run the **Build release** workflow (`patch` / `minor` / `major`) — it tags,
builds multi-arch images and pushes `pharndt/mqtt-mail:vX.Y.Z` to Docker Hub.
Then bump `image.tag` in
`homeserver-gitops/cluster/charts/mqtt/mail/chart/values.yaml` and run
`cluster/charts/mqtt/mail/install.sh`.
