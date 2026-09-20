package main

import (
	"context"
	"encoding/json"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mqtt-home/mqtt-alerting/config"
	"github.com/mqtt-home/mqtt-alerting/mail"
	"github.com/mqtt-home/mqtt-alerting/version"
	"github.com/mqtt-home/mqtt-alerting/web"
	"github.com/philipparndt/go-logger"
	"github.com/philipparndt/mqtt-gateway/mqtt"
)

var (
	engine    *mail.Engine
	mailer    *mail.Mailer
	webServer *web.WebServer
)

// publishStatus mirrors the alert state onto `<topic>/status`. Retained, so a
// consumer that subscribes later immediately sees the current state.
//
// It only hands the status over. Publishing waits for the broker's
// acknowledgement, and the engine calls this from wherever an alert changed —
// including, indirectly, from inside an MQTT message callback, where waiting
// for an acknowledgement deadlocks the client (v0.2.0 wedged itself that way
// after four hours). Only the newest status matters, so a slow broker makes
// this skip statuses instead of queueing them.
func publishStatus(status mail.Status) {
	select {
	case <-statusQueue: // drop the one nobody picked up yet
	default:
	}
	select {
	case statusQueue <- status:
	default: // a concurrent caller got there first; its status is just as new
	}
}

var statusQueue = make(chan mail.Status, 1)

func runStatusPublisher(stop <-chan struct{}) {
	for {
		select {
		case status := <-statusQueue:
			sendStatus(status)
		case <-stop:
			return
		}
	}
}

func sendStatus(status mail.Status) {
	cfg := config.Get()
	topic := cfg.MQTT.Topic + "/status"

	data, err := json.Marshal(status)
	if err != nil {
		logger.Error("Failed to marshal status", "error", err)
		return
	}

	mqtt.PublishAbsolute(topic, string(data), cfg.MQTT.Retain)
	logger.Debug("Published status", "topic", topic)

	if webServer != nil {
		webServer.BroadcastStatus(status)
	}
}

// publishAvailability publishes the connection state to `<topic>/availability`.
// Always retained: consumers use it to distinguish "device says off" from
// "bridge lost the device", instead of trusting a stale retained status.
func publishAvailability(online bool) {
	cfg := config.Get()
	payload := "offline"
	if online {
		payload = "online"
	}
	mqtt.PublishAbsolute(cfg.MQTT.Topic+"/availability", payload, true)
}

// subscribeToCommands listens on `<topic>/set` for JSON commands.
func subscribeToCommands() {
	cfg := config.Get()
	topic := cfg.MQTT.Topic + "/set"

	logger.Info("Subscribing to MQTT commands", "topic", topic)

	mqtt.Subscribe(topic, func(topic string, payload []byte) {
		logger.Debug("Received MQTT command", "topic", topic, "payload", string(payload))

		var cmd struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(payload, &cmd); err != nil {
			logger.Error("Failed to parse command", "error", err)
			return
		}

		go dispatchCommand(cmd.Action)
	})
}

// dispatchCommand runs one command off the MQTT callback goroutine. A panic here
// must never take the process down — the broker callback has no recovery.
func dispatchCommand(action string) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Panic in command processing", "panic", r)
		}
	}()

	var err error
	switch action {
	case "test":
		err = mailer.SendTest(time.Now())
	default:
		logger.Warn("Unknown action", "action", action)
		return
	}

	if err != nil {
		logger.Error("Command failed", "action", action, "error", err)
	}
}

// subscribeToRules feeds every topic a rule watches into the engine.
//
// The callback does nothing but enqueue: the MQTT client delivers messages one
// at a time and cannot read anything else from the broker (acknowledgements,
// pings) while a callback runs. Rule evaluation can publish and send mail, so
// it happens on a worker.
func subscribeToRules() {
	for _, filter := range engine.Subscriptions() {
		logger.Info("Watching", "topic", filter)
		mqtt.Subscribe(filter, func(topic string, payload []byte) {
			msg := inbound{topic: topic, payload: append([]byte(nil), payload...), at: time.Now()}
			select {
			case inboundQueue <- msg:
			default:
				// Falling this far behind means the worker is stuck; the
				// liveness probe will notice. Never block the client.
				dropped.Add(1)
			}
		})
	}
}

type inbound struct {
	topic   string
	payload []byte
	at      time.Time
}

var (
	inboundQueue = make(chan inbound, 4096)
	dropped      atomic.Int64
)

func runInboundWorker(stop <-chan struct{}) {
	for {
		select {
		case msg := <-inboundQueue:
			engine.HandleMessage(msg.topic, msg.payload, msg.at)
		case <-stop:
			return
		}
	}
}

func main() {
	logger.Init("info", logger.Logger())
	logger.Info("mqtt-alerting", "version", version.Info())
	initPprof()

	if len(os.Args) < 2 {
		logger.Error("No configuration file specified")
		os.Exit(1)
	}

	// `mqtt-alerting --check config.json` validates the rules and exits, without
	// touching MQTT or SMTP. A rule that does not compile stops the service at
	// startup, so check before rolling out.
	if os.Args[1] == "--check" {
		os.Exit(checkConfig(os.Args[2:]))
	}

	configFile := os.Args[1]
	logger.Info("Configuration file", "path", configFile)

	cfg, err := config.LoadConfig(configFile)
	if err != nil {
		logger.Error("Failed to load configuration", "error", err)
		return
	}

	logger.SetLevel(cfg.LogLevel)

	// MQTT first — the status callback publishes as soon as it fires.
	mqtt.Start(cfg.MQTT, "alerting_mqtt")

	// Seed a retained offline before connecting, so the availability topic is
	// never absent and consumers start from a safe default.
	publishAvailability(false)

	engine, err = mail.NewEngine(cfg.Mail.Rules, time.Now())
	if err != nil {
		// A rule that does not compile would silently watch nothing; refuse to
		// start so the rollout fails where someone sees it.
		logger.Error("Invalid rules", "error", err)
		os.Exit(1)
	}
	logger.Info("Rules loaded", "count", len(cfg.Mail.Rules))

	mailer = mail.NewMailer(cfg.Mail)
	mailer.OnChange(engine.NotifyStatus)
	engine.OnEvent(mailer.Enqueue)
	engine.SetMailStats(mailer.Stats)
	mailer.SetSnapshot(engine.GetStatus)
	engine.SetDroppedCounter(dropped.Load)
	engine.AddStatusChangeListener(publishStatus)

	publishAvailability(true)
	publishStatus(engine.GetStatus())
	subscribeToCommands()
	subscribeToRules()

	stop := make(chan struct{})
	go runStatusPublisher(stop)
	go runInboundWorker(stop)
	go engine.Run(5*time.Second, stop)
	go mailer.Run(5*time.Second, stop)

	if queries := engine.PromQLRules(); len(queries) > 0 {
		if cfg.Prometheus.URL == "" {
			// Rules that can never be evaluated would look like a healthy node.
			logger.Error("Rules of type promql need prometheus.url", "rules", len(queries))
			os.Exit(1)
		}
		logger.Info("Evaluating promql rules", "rules", len(queries), "url", cfg.Prometheus.URL, "interval", cfg.Prometheus.Interval.Std().String())
		go runPrometheusPoller(cfg.Prometheus, queries, stop)
	}

	if !cfg.Web.Enabled {
		logger.Info("Web interface is disabled in the configuration")
	} else {
		webServer = web.NewWebServer(engine, mailer)
		go func() {
			logger.Info("Web interface available", "url", "http://localhost:"+strconv.Itoa(cfg.Web.Port))
			if err := webServer.Start(cfg.Web.Port); err != nil {
				logger.Error("Failed to start web server", "error", err)
			}
		}()
	}

	logger.Info("Application ready")

	quitChannel := make(chan os.Signal, 1)
	signal.Notify(quitChannel, syscall.SIGINT, syscall.SIGTERM)
	<-quitChannel

	close(stop)
	// No "offline" here: in a rolling update the replacement is already up and
	// has said "online", and this process saying "offline" on its way out is
	// exactly the stale state this service exists to catch (it alerted on
	// itself). A crash is covered by the will on bridge/state.
	// Clean DISCONNECT: a planned shutdown must not fire the "offline" will.
	mqtt.Stop()
	logger.Info("Shutdown complete")
}

// runPrometheusPoller evaluates the promql rules. It has its own goroutine and
// its own clock: a slow or dead Prometheus must never hold up the MQTT side.
func runPrometheusPoller(pc config.PrometheusConfig, queries []mail.PromQLRule, stop <-chan struct{}) {
	client := &http.Client{Timeout: pc.Timeout.Std()}

	evaluate := func() {
		for _, q := range queries {
			ctx, cancel := context.WithTimeout(context.Background(), pc.Timeout.Std())
			samples, err := mail.QueryPrometheus(ctx, client, pc.URL, q.Query)
			cancel()
			if err != nil {
				logger.Warn("Prometheus query failed", "rule", q.Name, "error", err)
				engine.HandleQueryError(err, time.Now())
				continue
			}
			engine.HandleQueryResult(q.Name, samples, time.Now())

			if q.Watch == "" {
				continue
			}
			ctx, cancel = context.WithTimeout(context.Background(), pc.Timeout.Std())
			population, err := mail.QueryPrometheus(ctx, client, pc.URL, q.Watch)
			cancel()
			if err != nil {
				logger.Warn("Prometheus watch query failed", "rule", q.Name, "error", err)
				engine.HandleQueryError(err, time.Now())
				continue
			}
			engine.HandleWatchResult(q.Name, len(population), time.Now())
		}
	}

	evaluate()
	ticker := time.NewTicker(pc.Interval.Std())
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			evaluate()
		case <-stop:
			return
		}
	}
}

func checkConfig(args []string) int {
	if len(args) != 1 {
		logger.Error("Usage: mqtt-alerting --check <config.json>")
		return 2
	}
	cfg, err := config.LoadConfig(args[0])
	if err != nil {
		return 1
	}
	e, err := mail.NewEngine(cfg.Mail.Rules, time.Now())
	if err != nil {
		logger.Error("Invalid rules", "error", err)
		return 1
	}
	for _, r := range e.GetStatus().Rules {
		logger.Info("Rule ok", "name", r.Name, "type", r.Type, "check", r.Summary)
	}
	logger.Info("Configuration is valid", "rules", len(cfg.Mail.Rules), "subscriptions", len(e.Subscriptions()))
	return 0
}

// initPprof exposes pprof + expvar on :6060. Every bridge does this; the chart
// opens the port so `kubectl port-forward … 6060` works without a redeploy.
func initPprof() {
	go func() {
		http.ListenAndServe(":6060", nil)
	}()
}
