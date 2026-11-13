package provider

import (
	"fmt"
	"net/http"

	"github.com/alanhenschel/llmgate/internal/config"
)

// New builds the adapter for one configured provider instance.
func New(name string, c config.Provider, client *http.Client) (Provider, error) {
	switch c.Type {
	case config.TypeOpenAI:
		return NewOpenAI(name, c.Type, c.BaseURL, c.APIKey, true, client), nil
	case config.TypeOllama:
		return NewOpenAI(name, c.Type, c.BaseURL, c.APIKey, false, client), nil
	case config.TypeAnthropic:
		return NewAnthropic(name, c.BaseURL, c.APIKey, client), nil
	case config.TypeMock:
		return NewMock(name, c.Latency, c.ChunkDelay, c.FailRate), nil
	}
	return nil, fmt.Errorf("provider %q: unknown type %q", name, c.Type)
}
