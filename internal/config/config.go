// Package config loads and validates the llmgate YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider type identifiers.
const (
	TypeOpenAI    = "openai"
	TypeAnthropic = "anthropic"
	TypeOllama    = "ollama"
	TypeMock      = "mock"
)

// Config is the full gateway configuration.
type Config struct {
	Listen         string               `yaml:"listen"`
	RedisURL       string               `yaml:"redis_url"`
	RequestTimeout time.Duration        `yaml:"request_timeout"`
	Routes         map[string][]Target  `yaml:"routes"`
	Providers      map[string]Provider  `yaml:"providers"`
	CircuitBreaker CircuitBreaker       `yaml:"circuit_breaker"`
	Pricing        map[string]ModelCost `yaml:"pricing"`
	Keys           []Key                `yaml:"keys"`

	// Admin enables the /admin endpoints (LLMGATE_ADMIN=1).
	Admin bool `yaml:"-"`
}

// Target is one hop of a fallback chain. Provider is a key of Config.Providers.
type Target struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// Provider describes one named provider instance.
type Provider struct {
	// Type is openai|anthropic|ollama|mock. Defaults to the instance name.
	Type      string `yaml:"type"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`

	// Mock-only settings.
	Latency    time.Duration `yaml:"latency"`
	ChunkDelay time.Duration `yaml:"chunk_delay"`
	FailRate   float64       `yaml:"fail_rate"`

	// APIKey is resolved from APIKeyEnv at load time.
	APIKey string `yaml:"-"`
}

// CircuitBreaker holds per-provider breaker settings.
type CircuitBreaker struct {
	FailureThreshold int           `yaml:"failure_threshold"`
	OpenFor          time.Duration `yaml:"open_for"`
}

// ModelCost overrides the built-in price table (USD per 1M tokens).
type ModelCost struct {
	InputPer1M  float64 `yaml:"input_per_1m"`
	OutputPer1M float64 `yaml:"output_per_1m"`
}

// Key is a gateway API key with its quotas. Zero limits mean unlimited.
type Key struct {
	Name         string  `yaml:"name"`
	KeyEnv       string  `yaml:"key_env"`
	Key          string  `yaml:"key"`
	TokensPerDay int64   `yaml:"tokens_per_day"`
	USDPerDay    float64 `yaml:"usd_per_day"`
	RPM          int     `yaml:"rpm"`
}

var defaultBaseURL = map[string]string{
	TypeOpenAI:    "https://api.openai.com",
	TypeAnthropic: "https://api.anthropic.com",
	TypeOllama:    "http://localhost:11434",
}

// Load reads the file at path (or $LLMGATE_CONFIG, or config.yaml when path is
// empty), applies env overrides and validates the result.
func Load(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("LLMGATE_CONFIG")
	}
	if path == "" {
		path = "config.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse decodes, applies env overrides and validates a YAML document.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.finalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) finalize() error {
	if v := os.Getenv("LLMGATE_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("LLMGATE_REDIS_URL"); v != "" {
		c.RedisURL = v
	}
	c.Admin = os.Getenv("LLMGATE_ADMIN") == "1"

	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.RedisURL == "" {
		c.RedisURL = "redis://localhost:6379/0"
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 60 * time.Second
	}
	if c.CircuitBreaker.FailureThreshold <= 0 {
		c.CircuitBreaker.FailureThreshold = 5
	}
	if c.CircuitBreaker.OpenFor <= 0 {
		c.CircuitBreaker.OpenFor = 30 * time.Second
	}

	var errs []error
	if len(c.Providers) == 0 {
		errs = append(errs, errors.New("no providers configured"))
	}
	for name, p := range c.Providers {
		if p.Type == "" {
			p.Type = name
		}
		switch p.Type {
		case TypeOpenAI, TypeAnthropic, TypeOllama:
			if p.BaseURL == "" {
				p.BaseURL = defaultBaseURL[p.Type]
			}
			p.BaseURL = strings.TrimRight(p.BaseURL, "/")
			if p.APIKeyEnv != "" {
				p.APIKey = os.Getenv(p.APIKeyEnv)
			}
		case TypeMock:
			if p.FailRate < 0 || p.FailRate > 1 {
				errs = append(errs, fmt.Errorf("provider %q: fail_rate must be in [0,1]", name))
			}
		default:
			errs = append(errs, fmt.Errorf("provider %q: unknown type %q", name, p.Type))
		}
		c.Providers[name] = p
	}

	if len(c.Routes) == 0 {
		errs = append(errs, errors.New("no routes configured"))
	}
	for name, chain := range c.Routes {
		if len(chain) == 0 {
			errs = append(errs, fmt.Errorf("route %q: empty chain", name))
		}
		for i, t := range chain {
			if _, ok := c.Providers[t.Provider]; !ok {
				errs = append(errs, fmt.Errorf("route %q hop %d: unknown provider %q", name, i, t.Provider))
			}
			if t.Model == "" {
				errs = append(errs, fmt.Errorf("route %q hop %d: model is empty", name, i))
			}
		}
	}

	seen := map[string]bool{}
	for i := range c.Keys {
		k := &c.Keys[i]
		if k.Name == "" {
			errs = append(errs, fmt.Errorf("keys[%d]: name is empty", i))
		} else if seen[k.Name] {
			errs = append(errs, fmt.Errorf("keys[%d]: duplicate name %q", i, k.Name))
		}
		seen[k.Name] = true
		if k.Key == "" && k.KeyEnv != "" {
			k.Key = os.Getenv(k.KeyEnv)
		}
		if k.Key == "" {
			errs = append(errs, fmt.Errorf("key %q: empty secret (set env %q or key)", k.Name, k.KeyEnv))
		}
		if k.TokensPerDay < 0 || k.USDPerDay < 0 || k.RPM < 0 {
			errs = append(errs, fmt.Errorf("key %q: limits must be >= 0", k.Name))
		}
	}
	if len(c.Keys) == 0 {
		errs = append(errs, errors.New("no keys configured"))
	}
	return errors.Join(errs...)
}
