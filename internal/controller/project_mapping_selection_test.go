package controller

import (
	"reflect"
	"testing"
)

func TestScopedAgentIdentifier(t *testing.T) {
	testCases := []struct {
		name       string
		identifier string
		want       string
	}{
		{name: "empty", identifier: "", want: ""},
		{name: "whitespace", identifier: "  ", want: ""},
		{name: "trims", identifier: "  my-agent  ", want: "my-agent"},
		{name: "raw", identifier: "my-agent", want: "my-agent"},
		{name: "prefixed", identifier: "org.my-agent", want: "org.my-agent"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := scopedAgentIdentifier(testCase.identifier); got != testCase.want {
				t.Fatalf("scopedAgentIdentifier(%q) = %q, want %q", testCase.identifier, got, testCase.want)
			}
		})
	}
}

func TestScopedPathAgentIdentifierCandidates(t *testing.T) {
	testCases := []struct {
		name       string
		scope      string
		identifier string
		want       []string
	}{
		{name: "empty", scope: "ORG", identifier: " ", want: nil},
		{name: "org", scope: "ORG", identifier: "hubagent", want: []string{"org.hubagent", "hubagent"}},
		{name: "account", scope: "ACCOUNT", identifier: "hubagent", want: []string{"account.hubagent", "hubagent"}},
		{name: "prefixed", scope: "ORG", identifier: "org.hubagent", want: []string{"org.hubagent", "hubagent"}},
		{name: "project", scope: "PROJECT", identifier: "hubagent", want: []string{"hubagent"}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got := scopedPathAgentIdentifierCandidates(testCase.scope, testCase.identifier)
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("unexpected candidates: got %#v, want %#v", got, testCase.want)
			}
		})
	}
}

func TestProjectIdentifierForAgentScope(t *testing.T) {
	testCases := []struct {
		name      string
		scope     string
		projectID string
		want      string
	}{
		{name: "project", scope: "PROJECT", projectID: "my-project", want: "my-project"},
		{name: "project case insensitive and trimmed", scope: "project", projectID: " my-project ", want: "my-project"},
		{name: "org", scope: "ORG", projectID: "my-project", want: ""},
		{name: "account", scope: "ACCOUNT", projectID: "my-project", want: ""},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got := projectIdentifierForAgentScope(testCase.scope, testCase.projectID)
			if got != testCase.want {
				t.Fatalf("unexpected project identifier: got %q, want %q", got, testCase.want)
			}
		})
	}
}
