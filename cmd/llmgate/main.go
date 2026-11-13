// Command llmgate is an OpenAI-compatible LLM gateway with provider fallback,
// circuit breaking, per-key budgets and Prometheus metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"

	"github.com/alanhenschel/llmgate/internal/api"
	"github.com/alanhenschel/llmgate/internal/budget"
	"github.com/alanhenschel/llmgate/internal/config"
	"github.com/alanhenschel/llmgate/internal/metrics"
	"github.com/alanhenschel/llmgate/internal/pricing"
	"github.com/alanhenschel/llmgate/internal/provider"
	"github.com/alanhenschel/llmgate/internal/router"
)

const shutdownGrace = 30 * time.Second

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	configPath := flag.String("config", "", "path to config YAML (default $LLMGATE_CONFIG or config.yaml)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*configPath, log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis_url: %w", err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()

	m := metrics.New()
	providers := make(map[string]provider.Provider, len(cfg.Providers))
	mocks := map[string]*provider.Mock{}
	client := provider.DefaultClient()
	for name, pc := range cfg.Providers {
		p, err := provider.New(name, pc, client)
		if err != nil {
			return err
		}
		providers[name] = p
		if mk, ok := p.(*provider.Mock); ok {
			mocks[name] = mk
		}
		if pc.APIKeyEnv != "" && pc.APIKey == "" {
			log.Warn("provider api key env is empty; calls will fall through", "provider", name, "env", pc.APIKeyEnv)
		}
	}
	rt, err := router.New(cfg, providers, m, log)
	if err != nil {
		return err
	}

	prices := map[string]pricing.Price{}
	for model, c := range cfg.Pricing {
		prices[model] = pricing.Price{InputPer1M: c.InputPer1M, OutputPer1M: c.OutputPer1M}
	}
	srv := api.New(api.Deps{
		Config: cfg, Router: rt, Limiter: budget.NewStore(rdb), Metrics: m,
		Prices: pricing.New(prices), Mocks: mocks, Log: log,
	})

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: it would cut long SSE streams.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("llmgate listening", "addr", cfg.Listen, "routes", rt.Routes(), "admin", cfg.Admin)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	stop() // a second signal kills the process immediately
	log.Info("shutting down", "grace", shutdownGrace.String())
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := httpSrv.Shutdown(sctx); err != nil {
		log.Warn("graceful shutdown timed out, closing connections", "error", err)
		_ = httpSrv.Close()
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("bye")
	return nil
}

// healthcheck probes the local /healthz; used as container HEALTHCHECK in
// distroless images that ship neither curl nor wget.
func healthcheck() int {
	addr := os.Getenv("LLMGATE_LISTEN")
	if addr == "" {
		addr = listenFromConfig()
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "8080"
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

// listenFromConfig reads only the `listen` key so the probe does not depend on
// the full config being valid or on secrets being present.
func listenFromConfig() string {
	path := os.Getenv("LLMGATE_CONFIG")
	if path == "" {
		path = "config.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ":8080"
	}
	var c struct {
		Listen string `yaml:"listen"`
	}
	if yaml.Unmarshal(data, &c) != nil || c.Listen == "" {
		return ":8080"
	}
	return c.Listen
}
