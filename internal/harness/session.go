package harness

import (
	"context"
	"fmt"
	"net/http"
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

	return NewSessionWithClient(apiKey, nextgen.NewAPIClient(newSDKConfiguration()))
}

// newSDKConfiguration is the shared production transport policy for all APIs.
func newSDKConfiguration() *nextgen.Configuration {
	cfg := nextgen.NewConfiguration()
	// Credentials come exclusively from the namespace-local session, never
	// from the SDK's Terraform-oriented environment defaults.
	cfg.ApiKey = ""
	cfg.AccountId = ""
	// Bypass the SDK's body-dumping debug transport (TF_LOG=DEBUG can expose
	// private keys in responses). Share the standard connection pool between
	// short-lived sessions; only controller-safe API errors are logged.
	cfg.HTTPClient.Logger = nil
	cfg.HTTPClient.HTTPClient.Transport = http.DefaultTransport
	// x-api-key is a custom header that net/http can forward on redirects.
	// Harness API redirects are not needed; never forward provisioning keys.
	cfg.HTTPClient.HTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// controller-runtime owns retries and rate limiting. SDK retries can block
	// the sole reconcile worker for minutes during a Harness 5xx response.
	cfg.HTTPClient.RetryMax = 0
	cfg.HTTPClient.HTTPClient.Timeout = DefaultHTTPTimeout

	return cfg
}

// NewSessionWithClient builds a session around a configured Harness SDK
// client. Resource packages use this to keep transport details out of the
// shared authentication contract.
func NewSessionWithClient(apiKey string, sdkClient *nextgen.APIClient) (*Session, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("harness API key is empty")
	}
	if sdkClient == nil {
		return nil, fmt.Errorf("harness SDK client is nil")
	}
	return &Session{client: sdkClient, apiKey: apiKey}, nil
}

// Client returns the configured Harness SDK client.
func (s *Session) Client() *nextgen.APIClient {
	return s.client
}

// AuthContext adds this session's API key to a request context.
func (s *Session) AuthContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, nextgen.ContextAPIKey, nextgen.APIKey{
		Key: s.apiKey,
	})
}
