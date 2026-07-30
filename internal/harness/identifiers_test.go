package harness

import (
	"reflect"
	"testing"
)

func TestScopedIdentifierCandidates(t *testing.T) {
	tests := []struct {
		name       string
		scope      string
		identifier string
		want       []string
	}{
		{name: "empty", scope: "ORG", identifier: " ", want: nil},
		{name: "org", scope: "ORG", identifier: "agent", want: []string{"org.agent", "agent"}},
		{name: "account", scope: "ACCOUNT", identifier: "agent", want: []string{"account.agent", "agent"}},
		{name: "prefixed", scope: "ORG", identifier: "org.agent", want: []string{"org.agent", "agent"}},
		{name: "project", scope: "PROJECT", identifier: "agent", want: []string{"agent"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ScopedIdentifierCandidates(test.scope, test.identifier)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("candidates = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestProjectIdentifierForScope(t *testing.T) {
	tests := []struct {
		scope     string
		projectID string
		want      string
	}{
		{scope: "PROJECT", projectID: " project ", want: "project"},
		{scope: "project", projectID: "project", want: "project"},
		{scope: "ORG", projectID: "project", want: ""},
		{scope: "ACCOUNT", projectID: "project", want: ""},
	}

	for _, test := range tests {
		if got := ProjectIdentifierForScope(test.scope, test.projectID); got != test.want {
			t.Errorf("scope %q project = %q, want %q", test.scope, got, test.want)
		}
	}
}
