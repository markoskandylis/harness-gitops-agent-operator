package harness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestValidateEndpointAcceptsOnlyPlainBaseURLs(t *testing.T) {
	for _, valid := range []string{
		DefaultEndpoint,
		"https://app3.harness.io/gateway",
		"http://127.0.0.1:8080",
	} {
		if err := ValidateEndpoint(valid); err != nil {
			t.Fatalf("%q rejected: %v", valid, err)
		}
	}
	for _, invalid := range []string{
		"",
		"app.harness.io/gateway",
		"ftp://app.harness.io/gateway",
		"https://",
		"https://user:secret@app.harness.io/gateway",
		"https://app.harness.io/gateway?x=1",
		"https://app.harness.io/gateway#fragment",
	} {
		if err := ValidateEndpoint(invalid); err == nil {
			t.Fatalf("%q accepted", invalid)
		}
	}
}

func TestSessionUsesConfiguredEndpointNotEnvironment(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	t.Setenv("HARNESS_ENDPOINT", "https://ambient.example.invalid/gateway")

	session, err := NewSessionWithEndpoint("test-key", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, response, err := session.Client().AgentApi.AgentServiceForServerGet(
		session.AuthContext(context.Background()), "agent", "account", nil)
	if err == nil || response == nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("expected the configured server's 404, got response=%v err=%v", response, err)
	}
	if calls.Load() != 1 {
		t.Fatal("request did not reach the configured endpoint")
	}

	if _, err := NewSessionWithEndpoint("test-key", ""); err != nil {
		t.Fatalf("empty endpoint must fall back to the default: %v", err)
	}
	if _, err := NewSessionWithEndpoint("test-key", "ftp://app.harness.io"); err == nil {
		t.Fatal("invalid endpoint accepted")
	}
}
