package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/philipparndt/go-logger"
	"github.com/philipparndt/mqtt-gateway/config"
)

var cfg Config

type Config struct {
	MQTT     config.MQTTConfig `json:"mqtt"`
	Mail     MailConfig        `json:"mail"`
	Web      WebConfig         `json:"web"`
	LogLevel string            `json:"loglevel,omitempty"`
}

// MailConfig holds the SMTP account and the alert rules.
// Secrets come in as ${ENV_VAR} placeholders and are substituted by
// config.ReplaceEnvVariables before unmarshalling.
type MailConfig struct {
	SMTP SMTPConfig `json:"smtp"`
	// SubjectPrefix is prepended to every subject, e.g. "[smarthome]".
	SubjectPrefix string `json:"subject_prefix,omitempty"`
	// BatchSeconds is how long alerts are collected before one mail goes out,
	// so a broker restart produces one mail instead of twenty. Defaults to 30.
	BatchSeconds int `json:"batch_seconds,omitempty"`
	// MaxMailsPerHour delays (never drops) mails beyond this budget. Defaults to 12.
	MaxMailsPerHour int          `json:"max_mails_per_hour,omitempty"`
	Rules           []RuleConfig `json:"rules"`
}

type SMTPConfig struct {
	Enabled  bool   `json:"enabled"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
	// To accepts one address or a comma separated list.
	To string `json:"to"`
}

// Rule types.
const (
	RuleState   = "state"
	RuleCount   = "count"
	RuleSilence = "silence"
)

// RuleConfig is one monitoring rule. A rule is evaluated separately for every
// concrete topic its filters match, so a single wildcard rule covers a fleet.
type RuleConfig struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Type is "state" (condition holds for `for`), "count" (condition matched
	// `count` times within `within`) or "silence" (no message for `for`).
	Type string `json:"type"`
	// Topics are MQTT filters, wildcards allowed. Exclude uses the same syntax.
	Topics  []string `json:"topics"`
	Exclude []string `json:"exclude,omitempty"`

	Condition *ConditionConfig `json:"condition,omitempty"`

	For    Duration `json:"for,omitempty"`
	Count  int      `json:"count,omitempty"`
	Within Duration `json:"within,omitempty"`

	// Repeat re-sends a still-firing alert at this interval. Zero: never.
	Repeat Duration `json:"repeat,omitempty"`
	// Recovery controls the "resolved" mail. Defaults to true.
	Recovery *bool `json:"recovery,omitempty"`
}

// ConditionConfig tests the payload, or one field of a JSON payload. All
// operators that are set must hold.
type ConditionConfig struct {
	// Field is a dot path into a JSON payload ("state", "battery.level").
	// Empty: the raw payload is the value.
	Field     string   `json:"field,omitempty"`
	Equals    *string  `json:"equals,omitempty"`
	NotEquals *string  `json:"not_equals,omitempty"`
	Regex     string   `json:"regex,omitempty"`
	LT        *float64 `json:"lt,omitempty"`
	GT        *float64 `json:"gt,omitempty"`
}

// Duration is a time.Duration that reads "5m" / "24h" from JSON.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"5m\": %w", err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

type WebConfig struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port"`
	// LivenessGraceSeconds is how long the bridge may stay unhealthy before
	// /api/livez starts failing. Defaults to 240.
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
	if cfg.Mail.BatchSeconds == 0 {
		cfg.Mail.BatchSeconds = 30
	}
	if cfg.Mail.MaxMailsPerHour == 0 {
		cfg.Mail.MaxMailsPerHour = 12
	}
	if cfg.Mail.SMTP.Port == 0 {
		cfg.Mail.SMTP.Port = 587
	}
	if cfg.Web.Port == 0 {
		cfg.Web.Port = 8080
	}

	return cfg, nil
}

func Get() Config {
	return cfg
}
