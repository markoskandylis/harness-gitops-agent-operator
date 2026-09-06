package harness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestProductionSessionConfigurationDoesNotInheritCredentialsOrDumpBodies(t *testing.T) {
	t.Setenv("TF_LOG", "DEBUG")
	t.Setenv("HARNESS_PLATFORM_API_KEY", "global-test-credential")
	t.Setenv("HARNESS_ACCOUNT_ID", "global-test-account")
	t.Setenv("HARNESS_ENDPOINT", "https://ambient.example.invalid/gateway")
	cfg := newSDKConfiguration(DefaultEndpoint)
	if cfg.ApiKey != "" || cfg.AccountId != "" {
		t.Fatal("production configuration inherited ambient credentials")
	}
	if cfg.BasePath != DefaultEndpoint {
		t.Fatal("production configuration inherited an ambient endpoint")
	}
	if cfg.HTTPClient.Logger != nil || cfg.HTTPClient.HTTPClient.Transport != http.DefaultTransport {
		t.Fatal("production transport can use SDK request/response logging")
	}
	if cfg.HTTPClient.RetryMax != 0 || cfg.HTTPClient.HTTPClient.Timeout != DefaultHTTPTimeout {
		t.Fatal("production transport lost controller-owned retry or timeout policy")
	}
	if next := newSDKConfiguration(DefaultEndpoint); next.HTTPClient.HTTPClient.Transport != cfg.HTTPClient.HTTPClient.Transport {
		t.Fatal("per-reconcile sessions must reuse the connection pool")
	}
	if _, err := NewSession(" "); err == nil {
		t.Fatal("ambient credentials rescued an empty local credential")
	}
}

func TestProductionSessionDoesNotForwardAPIKeyOnRedirect(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(destination.Close)
	var sourceCalls atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sourceCalls.Add(1)
		if req.Header.Get("x-api-key") != "namespace-local-test-key" {
			t.Error("request did not use its namespace-local credential")
		}
		http.Redirect(w, req, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)
	session, err := NewSession("namespace-local-test-key")
	if err != nil {
		t.Fatal(err)
	}
	session.Client().ChangeBasePath(source.URL)
	_, response, err := session.Client().AgentApi.AgentServiceForServerGet(
		session.AuthContext(context.Background()), "test-agent", "test-account", nil)
	if err == nil || response == nil || response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatal("redirect should remain an API error")
	}
	if sourceCalls.Load() != 1 || destinationCalls.Load() != 0 {
		t.Fatal("API request was retried or followed a redirect")
	}
}
