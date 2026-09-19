package config

import (
	"encoding/json"
	"os"

	"github.com/philipparndt/go-logger"
	"github.com/philipparndt/mqtt-gateway/config"
)

var cfg Config

type Config struct {
	MQTT     config.MQTTConfig `json:"mqtt"`
	Mail MailConfig  `json:"mail"`
	Web      WebConfig         `json:"web"`
	LogLevel string            `json:"loglevel,omitempty"`
}

// MailConfig holds everything needed to talk to the device / cloud API.
// Secrets come in as ${ENV_VAR} placeholders and are substituted by
// config.ReplaceEnvVariables before unmarshalling.
type MailConfig struct {
	Host            string `json:"host,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	PollingInterval int    `json:"polling_interval,omitempty"`
}

type WebConfig struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port"`
	// LivenessGraceSeconds is how long the bridge may stay unhealthy (not
	// connected to the device) before /api/livez starts failing. Defaults to 240.
	LivenessGraceSeconds int `json:"liveness_grace_seconds,omitempty"`
}

func LoadConfig(file string) (Config, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		logger.Error("Error reading config file", "error", err)
		return Config{}, err
	}

	data = config.ReplaceEnvVariables(data)

	err = json.Unmarshal(data, &cfg)
	if err != nil {
		logger.Error("Unmarshaling JSON", "error", err)
		return Config{}, err
	}

	// Defaults
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.Mail.PollingInterval == 0 {
		cfg.Mail.PollingInterval = 30
	}
	if cfg.Web.Port == 0 {
		cfg.Web.Port = 8080
	}

	return cfg, nil
}

func Get() Config {
	return cfg
}
