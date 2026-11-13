package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   int
	}{
		{"healthy", http.StatusOK, 0},
		{"unhealthy", http.StatusServiceUnavailable, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("path %s", r.URL.Path)
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			t.Setenv("LLMGATE_LISTEN", ":"+u.Port())
			if got := healthcheck(); got != tt.want {
				t.Fatalf("healthcheck() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestHealthcheckConnectionRefused(t *testing.T) {
	t.Setenv("LLMGATE_LISTEN", "127.0.0.1:1")
	if got := healthcheck(); got != 1 {
		t.Fatalf("got %d", got)
	}
}

func TestListenFromConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte("listen: \":9191\"\nunknown_field: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLMGATE_CONFIG", p)
	if got := listenFromConfig(); got != ":9191" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("LLMGATE_CONFIG", filepath.Join(dir, "missing.yaml"))
	if got := listenFromConfig(); got != ":8080" {
		t.Fatalf("got %q", got)
	}
}
