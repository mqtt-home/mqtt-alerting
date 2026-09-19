package main

import (
	"encoding/json"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/mqtt-home/mqtt-mail/config"
	"github.com/mqtt-home/mqtt-mail/mail"
	"github.com/mqtt-home/mqtt-mail/version"
	"github.com/mqtt-home/mqtt-mail/web"
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
func publishStatus(status mail.Status) {
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
func subscribeToRules() {
	for _, filter := range engine.Subscriptions() {
		logger.Info("Watching", "topic", filter)
		mqtt.Subscribe(filter, func(topic string, payload []byte) {
			engine.HandleMessage(topic, payload, time.Now())
		})
	}
}

func main() {
	logger.Init("info", logger.Logger())
	logger.Info("mqtt-mail", "version", version.Info())
	initPprof()

	if len(os.Args) < 2 {
		logger.Error("No configuration file specified")
		os.Exit(1)
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
	mqtt.Start(cfg.MQTT, "mail_mqtt")

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
	engine.AddStatusChangeListener(publishStatus)

	publishAvailability(true)
	publishStatus(engine.GetStatus())
	subscribeToCommands()
	subscribeToRules()

	stop := make(chan struct{})
	go engine.Run(5*time.Second, stop)
	go mailer.Run(5*time.Second, stop)

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
	publishAvailability(false)
	logger.Info("Shutdown complete")
}

// initPprof exposes pprof + expvar on :6060. Every bridge does this; the chart
// opens the port so `kubectl port-forward … 6060` works without a redeploy.
func initPprof() {
	go func() {
		http.ListenAndServe(":6060", nil)
	}()
}
