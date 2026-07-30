package harness

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/harness/harness-go-sdk/harness/nextgen"
)

// DefaultHTTPTimeout caps one Harness API round trip.
const DefaultHTTPTimeout = 60 * time.Second

// Session contains the configured Harness SDK client and API key.
type Session struct {
	client *nextgen.APIClient
	apiKey string
}

// NewSession builds a Harness SDK session from an API key supplied by the
// controller. Kubernetes Secret lookup and namespace policy stay outside this
// leaf package.
func NewSession(apiKey string) (*Session, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("harness API key is empty")
	}

	cfg := nextgen.NewConfiguration()
	// controller-runtime owns retries and rate limiting. SDK retries can block
	// the sole reconcile worker for minutes during a Harness 5xx response.
	cfg.HTTPClient.RetryMax = 0
	cfg.HTTPClient.HTTPClient.Timeout = DefaultHTTPTimeout

	return &Session{
		client: nextgen.NewAPIClient(cfg),
		apiKey: apiKey,
	}, nil
}

func (s *Session) authContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, nextgen.ContextAPIKey, nextgen.APIKey{
		Key: s.apiKey,
	})
}
