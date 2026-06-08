package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Meta struct {
	AccessToken   string `yaml:"-"`
	AppSecret     string `yaml:"-"`
	VerifyToken   string `yaml:"-"`
	PhoneNumberID string `yaml:"-"`
	APIVersion    string `yaml:"api_version"`
}

type Redis struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type MessageReceiver struct {
	WebhookURL string `yaml:"webhook_url"`
}

type App struct {
	ID         string `yaml:"id"`
	Name       string `yaml:"name"`
	APIKeyHash string `yaml:"api_key_hash"`
	WebhookURL string `yaml:"webhook_url"`
	Rate       int    `yaml:"rate"` // per minute; 0 means use global
}

type Proxy struct {
	Port       int `yaml:"port"`
	GlobalRate int `yaml:"global_rate"`
}

type Config struct {
	Meta            Meta            `yaml:"meta"`
	Redis           Redis           `yaml:"redis"`
	Proxy           Proxy           `yaml:"proxy"`
	MessageReceiver MessageReceiver `yaml:"message_receiver"`
	Apps            []App           `yaml:"apps"`
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	// Secrets come exclusively from environment variables — never from the config file.
	cfg.Meta.AccessToken = os.Getenv("META_ACCESS_TOKEN")
	cfg.Meta.AppSecret = os.Getenv("META_APP_SECRET")
	cfg.Meta.VerifyToken = os.Getenv("META_VERIFY_TOKEN")
	cfg.Meta.PhoneNumberID = os.Getenv("META_PHONE_NUMBER_ID")

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Meta.AccessToken == "" {
		return errors.New("META_ACCESS_TOKEN env var is required")
	}
	if c.Meta.AppSecret == "" {
		return errors.New("META_APP_SECRET env var is required")
	}
	if c.Meta.VerifyToken == "" {
		return errors.New("META_VERIFY_TOKEN env var is required")
	}
	if c.Meta.PhoneNumberID == "" {
		return errors.New("META_PHONE_NUMBER_ID env var is required")
	}
	if c.Meta.APIVersion == "" {
		c.Meta.APIVersion = "v21.0"
	}
	if c.Redis.Addr == "" {
		return errors.New("redis.addr is required")
	}
	if c.Proxy.Port == 0 {
		c.Proxy.Port = 8080
	}
	if c.Proxy.GlobalRate <= 0 {
		return errors.New("proxy.global_rate must be greater than 0")
	}
	if c.MessageReceiver.WebhookURL == "" {
		return errors.New("message_receiver.webhook_url is required")
	}
	seen := make(map[string]bool)
	for i, app := range c.Apps {
		if app.ID == "" {
			return fmt.Errorf("apps[%d].id is required", i)
		}
		if seen[app.ID] {
			return fmt.Errorf("duplicate app id: %s", app.ID)
		}
		seen[app.ID] = true
		if app.APIKeyHash == "" {
			return fmt.Errorf("apps[%d].api_key_hash is required", i)
		}
		if app.WebhookURL == "" {
			return fmt.Errorf("apps[%d].webhook_url is required", i)
		}
	}
	return nil
}

// HashAPIKey returns the SHA-256 hex digest of a raw API key.
func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// AppByKeyHash finds an app whose stored hash matches the SHA-256 of rawKey.
func (c *Config) AppByKeyHash(rawKey string) (*App, bool) {
	h := HashAPIKey(rawKey)
	for i := range c.Apps {
		if c.Apps[i].APIKeyHash == h {
			return &c.Apps[i], true
		}
	}
	return nil, false
}

// AppByID finds an app by its ID.
func (c *Config) AppByID(id string) (*App, bool) {
	for i := range c.Apps {
		if c.Apps[i].ID == id {
			return &c.Apps[i], true
		}
	}
	return nil, false
}
