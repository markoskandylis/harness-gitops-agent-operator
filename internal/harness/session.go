package harness

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/harness/harness-go-sdk/harness/nextgen"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	return NewSessionWithClient(apiKey, nextgen.NewAPIClient(cfg))
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

// SessionFromSecret constructs a session from a caller-resolved Kubernetes
// Secret. Resource packages remain responsible for namespace and reference
// policy.
func SessionFromSecret(
	ctx context.Context,
	reader client.Reader,
	key client.ObjectKey,
) (*Session, error) {
	if reader == nil {
		return nil, fmt.Errorf("kubernetes Secret reader is nil")
	}

	secret := &corev1.Secret{}
	if err := reader.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	apiKey, ok := secret.Data["api_key"]
	if !ok || len(apiKey) == 0 {
		return nil, k8serrors.NewBadRequest("api_key not found in secret")
	}
	return NewSession(string(apiKey))
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
