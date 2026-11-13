package config

import (
	"strings"
	"testing"
	"time"
)

const valid = `
listen: ":9000"
routes:
  demo:
    - {provider: mock-a, model: m}
    - {provider: openai, model: gpt-4o-mini}
providers:
  mock-a: {type: mock, latency: 50ms, fail_rate: 0.5}
  openai: {api_key_env: TEST_OPENAI_KEY}
keys:
  - {name: demo, key_env: TEST_GW_KEY, tokens_per_day: 10, usd_per_day: 1.5, rpm: 3}
`

func TestParseValid(t *testing.T) {
	t.Setenv("TEST_GW_KEY", "secret")
	t.Setenv("TEST_OPENAI_KEY", "sk-x")
	t.Setenv("LLMGATE_REDIS_URL", "redis://r:1/0")
	t.Setenv("LLMGATE_ADMIN", "1")
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.RedisURL != "redis://r:1/0" || !c.Admin {
		t.Fatalf("unexpected top level: %+v", c)
	}
	if c.Providers["mock-a"].Latency != 50*time.Millisecond || c.Providers["mock-a"].Type != "mock" {
		t.Fatalf("mock: %+v", c.Providers["mock-a"])
	}
	o := c.Providers["openai"]
	if o.Type != "openai" || o.BaseURL != "https://api.openai.com" || o.APIKey != "sk-x" {
		t.Fatalf("openai: %+v", o)
	}
	if c.Keys[0].Key != "secret" || c.Keys[0].RPM != 3 {
		t.Fatalf("key: %+v", c.Keys[0])
	}
	if c.CircuitBreaker.FailureThreshold != 5 || c.RequestTimeout != time.Minute {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestParseErrors(t *testing.T) {
	t.Setenv("TEST_GW_KEY", "secret")
	tests := []struct {
		name, yaml, want string
	}{
		{"unknown provider in route", `
routes: {r: [{provider: nope, model: m}]}
providers: {mock: {}}
keys: [{name: a, key: k}]`, `unknown provider "nope"`},
		{"unknown type", `
routes: {r: [{provider: x, model: m}]}
providers: {x: {type: weird}}
keys: [{name: a, key: k}]`, `unknown type "weird"`},
		{"empty key secret", `
routes: {r: [{provider: mock, model: m}]}
providers: {mock: {}}
keys: [{name: a, key_env: DOES_NOT_EXIST_XYZ}]`, "empty secret"},
		{"unknown field", `
bogus: 1
routes: {r: [{provider: mock, model: m}]}
providers: {mock: {}}
keys: [{name: a, key: k}]`, "bogus"},
		{"bad fail rate", `
routes: {r: [{provider: mock, model: m}]}
providers: {mock: {fail_rate: 2}}
keys: [{name: a, key: k}]`, "fail_rate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestLoadExampleAndDemoConfigs(t *testing.T) {
	t.Setenv("LLMGATE_DEMO_KEY", "k")
	for _, path := range []string{"../../config.example.yaml", "../../deploy/config.demo.yaml"} {
		t.Run(path, func(t *testing.T) {
			c, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := c.Routes["demo"]; !ok {
				t.Fatal("route demo missing")
			}
			if c.Providers["mock-primary"].Type != TypeMock {
				t.Fatalf("mock-primary: %+v", c.Providers["mock-primary"])
			}
		})
	}
}
