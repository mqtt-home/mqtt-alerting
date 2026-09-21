# mqtt-alerting

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
| `promql` | a Prometheus query returns series for `for` — one alert per series | `query`, `for` (optional) |
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

### Prometheus rules

Not everything worth an alert is on MQTT: disk, memory and temperature of the
server, crash-looping pods, failed jobs, expiring certificates. Prometheus
already collects those, keeps their history, and would alert on them — into a
receiver nobody reads. A `promql` rule puts them into the same mails instead.

```json
"prometheus": { "url": "http://prometheus.example:9090", "interval": "1m" },

{
  "name": "node-disk-full",
  "type": "promql",
  "query": "max by (mountpoint) (100 - node_filesystem_avail_bytes / node_filesystem_size_bytes * 100) > 85",
  "watch": "max by (mountpoint) (node_filesystem_size_bytes)",
  "title": "Disk {mountpoint} is {value} % full",
  "resolved_title": "Disk {mountpoint} has room again",
  "for": "15m",
  "repeat": "24h"
}
```

- The query carries its own threshold. Every series it returns is one alert; a
  series that is no longer returned resolves it.
- Titles can use every label of the series (`{mountpoint}`, `{namespace}`,
  `{pod}`) and `{value}`, the sample rounded to one decimal.
- An alert is identified by its labels, without the ones that only say how the
  sample was collected (`job`, `instance`, `endpoint`, `service`, `container`).
  Labels added by a ServiceMonitor, like the *exporter's* `pod`, are not
  recognisable as such: aggregate them away (`max by (mountpoint) (...)`), or a
  restarted exporter resolves the alert and raises it again.
- **Give every rule a `watch` query.** A query with a threshold returns nothing
  while all is well — and also nothing when its metric no longer exists
  (exporter down, metric renamed, dropped at scrape time). `watch` is the same
  selection without the threshold. Its series count is what the rule covers, so
  the overview can say "all 53 fine" instead of a meaningless "0", and when it
  drops to zero for 15 minutes the built-in alert `promql-rule-blind` fires:
  *Rule pod-not-running sees no data*. Without `watch` a rule is listed as
  "coverage unknown".
- **A Prometheus that does not answer never resolves anything.** Not knowing is
  not the same as being fine. After 10 minutes without an answer the built-in
  alert `prometheus-unreachable` fires instead; it is added automatically as
  soon as there is a `promql` rule, and shows the error it got.

Rules that do not compile stop the service at startup rather than silently
watching nothing. Check a config before rolling it out:

```bash
mqtt-alerting --check config.json
```

## Example rules

[`production/config/config.example.json`](production/config/config.example.json)
is a working rule set, not a sketch: it is what runs in the author's house, minus
the rules that only name things in that house. Take what fits and delete the rest —
a rule whose topics never show up just watches nothing, and a `promql` rule whose
metric does not exist never returns a series.

| Rule | Fires when | Needs |
|---|---|---|
| `bridge-offline` | payload = "offline" for 10m | bridges that publish `<topic>/bridge/state` (every mqtt-gateway based bridge) |
| `zigbee2mqtt-offline` | state = "offline" for 10m | zigbee2mqtt |
| `device-unavailable` | payload = "offline" for 30m | bridges that publish `<topic>/<device>/availability` |
| `shelly-unreachable` | payload = "false" for 15m | Shelly devices with MQTT enabled |
| `reconnect-loop` | 8 matches within 15m | a bridge that mirrors its log to `<topic>/bridge/logs`; adapt the regex to its wording |
| `data-silent` | nothing under a filter for 30m | your own list of filters, one per service that publishes continuously |
| `battery-low` | battery < 15 for 1h | zigbee2mqtt |
| `nuki-battery-critical` | payload = "true" for 10m | Nuki Hub |
| `roborock-error` | error_code > 0 for 10m | roborock-mqtt |
| `roborock-stuck` | state is idle, paused, error, charging_problem or charger_disconnected for 15m | roborock-mqtt; catches a vacuum that gave up without an error code |
| `node-disk-full` | a filesystem is over 85 % for 15m | Prometheus + node-exporter |
| `node-disk-filling` | at the rate of the last 6h the disk is full within 4 days, for 1h | Prometheus + node-exporter |
| `node-memory` | memory is over 90 % for 15m | Prometheus + node-exporter |
| `node-temperature` | CPU is over 78 °C for 10m | Prometheus + node-exporter on a board with a `cpu-thermal` zone |
| `node-load` | 15-minute load is over 2 per core for 30m | Prometheus + node-exporter |
| `pod-restarting` | a pod restarted more than 3 times within an hour | Prometheus + kube-state-metrics |
| `pod-oom-killed` | a container was OOM-killed within the last hour | Prometheus + kube-state-metrics |
| `pod-not-running` | a pod is Pending or Unknown for 15m | Prometheus + kube-state-metrics |
| `deployment-unavailable` | a deployment has unavailable replicas for 15m | Prometheus + kube-state-metrics |
| `job-failed` | a job has failed, for 5m | Prometheus + kube-state-metrics |
| `certificate-expiring` | a certificate has under 14 days left, for 1h | Prometheus scraping cert-manager |
| `scrape-target-down` | a scrape target is down for 15m | Prometheus |

Thresholds and durations are starting points. Before trusting a `data-silent`
filter, measure the longest gap that service normally has and stay well above it;
a threshold that fires on a normal night teaches everyone to ignore the mails.

## Replay: try a rule on recorded history

Before a rule goes live, find out what it would have done:

```bash
mqtt-alerting --replay config.json --source https://mqtt-logger.example --from -14d --rules roborock-stuck
```

```
Read 1074130 messages, 673979 watched by the rules, from 5 day file(s), 171.0 MB in 8.2s

TIMELINE
  Mon 09-21 08:51  ALERT     roborock-stuck  Roborock carmen-og is stuck off its dock  value charger_disconnected
  Mon 09-21 10:38  RESOLVED  roborock-stuck  Roborock carmen-og is back on its dock ... after 1h 46m

SUMMARY
  rule            watched  alerts  reminders  longest  total firing
  roborock-stuck        2       5          0   4h 32m  13h 20m
```

It reads the history from an [mqtt-logger](https://github.com/philipparndt/mqtt-logger)
and runs it through the same engine the service uses, with the service's clock of
5-second ticks. It is offline: no broker connection, no mail.

- `--from` / `--to`: RFC3339, `YYYY-MM-DD`, `now` or relative (`-14d`, `-36h`).
- `--rules a,b`: replay only these. `--summary`: skip the timeline.
- A rule that never saw a single matching topic is named at the end — usually a
  typo in its topics.
- `promql` rules are skipped; they would need Prometheus history.
- State is rebuilt from `--from` on: a device that was already offline before it
  counts as offline from `--from`.
- With mqtt-logger v1.6.0 or later the history comes as one file per day, which
  takes seconds and costs the logger nothing. Older versions are read through
  their query API: correct, but about 100 times slower. `--events-api` forces
  that path, to cross-check the two.

## Mail

Mails are HTML with a plain-text alternative (`"plain_text": true` for text
only). Each one leads with a single sentence that is right even if nothing else
is read, then lists what went wrong, what is still not fixed and what works
again, other problems that are still open, and an overview of every rule with
what it watches and whether it is fine — so a mail about one broken thing also
says what works. `ui_url` adds a link to the dashboard.

Every rule is listed with how much of what it watches is fine — "all 19 fine",
"52 of 53 fine", "nothing seen yet" — next to a checkmark or the number firing.
"Nothing is firing" and "this rule is not looking at anything" read the same
otherwise. The dashboard and the `status` topic carry the same numbers
(`watching`, `ok`, `firing`, `pending`, `blind` per rule).

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
| `home/alerting/status` | published, retained | rules, current alerts, history, mail statistics |
| `home/alerting/availability` | published, retained | `online` \| `offline` |
| `home/alerting/set` | subscribed | `{"action": "test"}` sends a test mail |

The web UI shows the same status live and has a "Send test mail" button that
reports the SMTP error verbatim.

## Quick start

### Docker

```bash
docker run -d \
  -v /path/to/config:/var/lib/mqtt-alerting \
  -p 8080:8080 \
  pharndt/mqtt-alerting:latest
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
    "topic": "home/alerting",
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
builds multi-arch images and pushes `pharndt/mqtt-alerting:vX.Y.Z` to Docker Hub.
Then bump `image.tag` in
`homeserver-gitops/cluster/charts/mqtt/mail/chart/values.yaml` and run
`cluster/charts/mqtt/mail/install.sh`.
