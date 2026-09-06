package harness

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/harness/harness-go-sdk/harness/nextgen"
)

// DefaultHTTPTimeout caps one Harness API round trip.
const DefaultHTTPTimeout = 60 * time.Second

// DefaultEndpoint is the Harness API gateway used when no endpoint is configured.
const DefaultEndpoint = "https://app.harness.io/gateway"

// ValidateEndpoint accepts only an absolute http(s) base URL without embedded
// credentials, query, or fragment. The controller owns this value; it must
// never be taken from the pod environment.
func ValidateEndpoint(endpoint string) error {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return fmt.Errorf("harness endpoint is not a valid URL: %w", err)
	}
	switch {
	case parsed.Scheme != "https" && parsed.Scheme != "http":
		return fmt.Errorf("harness endpoint must use http or https")
	case parsed.Host == "":
		return fmt.Errorf("harness endpoint must include a host")
	case parsed.User != nil:
		return fmt.Errorf("harness endpoint must not embed credentials")
	case parsed.RawQuery != "" || parsed.Fragment != "":
		return fmt.Errorf("harness endpoint must not include a query or fragment")
	}
	return nil
}

// Session contains the configured Harness SDK client and API key.
type Session struct {
	client *nextgen.APIClient
	apiKey string
}

// NewSession builds a session against DefaultEndpoint. Kubernetes Secret
// lookup and namespace policy stay outside this leaf package.
func NewSession(apiKey string) (*Session, error) {
	return NewSessionWithEndpoint(apiKey, DefaultEndpoint)
}

// NewSessionWithEndpoint builds a session against an explicit Harness API
// gateway. An empty endpoint means DefaultEndpoint.
func NewSessionWithEndpoint(apiKey string, endpoint string) (*Session, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("harness API key is empty")
	}
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	if err := ValidateEndpoint(endpoint); err != nil {
		return nil, err
	}

	return NewSessionWithClient(apiKey, nextgen.NewAPIClient(newSDKConfiguration(endpoint)))
}

// newSDKConfiguration is the shared production transport policy for all APIs.
func newSDKConfiguration(endpoint string) *nextgen.Configuration {
	cfg := nextgen.NewConfiguration()
	// Credentials come exclusively from the namespace-local session, never
	// from the SDK's Terraform-oriented environment defaults.
	cfg.ApiKey = ""
	cfg.AccountId = ""
	// The SDK seeds BasePath from HARNESS_ENDPOINT. The controller owns the
	// endpoint, so the pod environment can never redirect provisioning keys.
	cfg.BasePath = endpoint
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
