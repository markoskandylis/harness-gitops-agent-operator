package controller

import (
	stderrors "errors"
	"testing"

	"github.com/harness/harness-go-sdk/harness/nextgen"
)

func TestSelectArgoProjectIDFromV2Mappings_DeterministicScopedMatch(t *testing.T) {
	mappings := []nextgen.V1AppProjectMappingV2{
		{
			AccountIdentifier: "acc",
			OrgIdentifier:     "org",
			ProjectIdentifier: "proj",
			ArgoProjectName:   "z-proj",
		},
		{
			AccountIdentifier: "acc",
			OrgIdentifier:     "org",
			ProjectIdentifier: "proj",
			ArgoProjectName:   "a-proj",
		},
	}

	got, err := selectArgoProjectIDFromV2Mappings(mappings, "acc", "org", "proj")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "a-proj" {
		t.Fatalf("expected deterministic lexical selection 'a-proj', got %q", got)
	}
}

func TestSelectArgoProjectIDFromV2Mappings_NotFound(t *testing.T) {
	_, err := selectArgoProjectIDFromV2Mappings(nil, "acc", "org", "proj")
	if !stderrors.Is(err, errArgoProjectMappingNotFound) {
		t.Fatalf("expected errArgoProjectMappingNotFound, got %v", err)
	}
}

func TestSelectArgoProjectIDFromV2Mappings_ScopeMismatch(t *testing.T) {
	mappings := []nextgen.V1AppProjectMappingV2{
		{
			AccountIdentifier: "acc",
			OrgIdentifier:     "other-org",
			ProjectIdentifier: "other-proj",
			ArgoProjectName:   "p1",
		},
	}

	_, err := selectArgoProjectIDFromV2Mappings(mappings, "acc", "org", "proj")
	if !stderrors.Is(err, errArgoProjectScopeMismatch) {
		t.Fatalf("expected errArgoProjectScopeMismatch, got %v", err)
	}
}

func TestSelectArgoProjectIDFromV1Mapping_DeterministicScopedMatch(t *testing.T) {
	mappings := map[string]nextgen.Servicev1Project{
		"z-proj": {
			OrgIdentifier:     "org",
			ProjectIdentifier: "proj",
		},
		"a-proj": {
			OrgIdentifier:     "org",
			ProjectIdentifier: "proj",
		},
	}

	got, err := selectArgoProjectIDFromV1Mapping(mappings, "org", "proj")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "a-proj" {
		t.Fatalf("expected deterministic lexical selection 'a-proj', got %q", got)
	}
}

func TestSelectArgoProjectIDFromV1Mapping_ScopeMismatch(t *testing.T) {
	mappings := map[string]nextgen.Servicev1Project{
		"proj-a": {
			OrgIdentifier:     "other-org",
			ProjectIdentifier: "other-proj",
		},
	}

	_, err := selectArgoProjectIDFromV1Mapping(mappings, "org", "proj")
	if !stderrors.Is(err, errArgoProjectScopeMismatch) {
		t.Fatalf("expected errArgoProjectScopeMismatch, got %v", err)
	}
}

func TestScopedAgentIdentifier_OrgScopeKeepsIdentifier(t *testing.T) {
	got := scopedAgentIdentifier("ORG", "my-agent")
	if got != "my-agent" {
		t.Fatalf("expected ORG scope identifier to remain unchanged, got %q", got)
	}
}

func TestScopedAgentIdentifier_AlreadyPrefixedUnchanged(t *testing.T) {
	got := scopedAgentIdentifier("ORG", "org.my-agent")
	if got != "org.my-agent" {
		t.Fatalf("expected existing prefixed identifier to remain unchanged, got %q", got)
	}
}

func TestScopedAgentIdentifier_OrgLikeIdentifierWithoutDotUnchanged(t *testing.T) {
	got := scopedAgentIdentifier("ORG", "orggitopsagent")
	if got != "orggitopsagent" {
		t.Fatalf("expected org-like identifier to remain unchanged, got %q", got)
	}
}

func TestScopedAgentIdentifier_ProjectScopeUnchanged(t *testing.T) {
	got := scopedAgentIdentifier("PROJECT", "my-agent")
	if got != "my-agent" {
		t.Fatalf("expected project scope identifier to remain unchanged, got %q", got)
	}
}

func TestScopedAgentIdentifier_AccountScopeKeepsIdentifier(t *testing.T) {
	got := scopedAgentIdentifier("ACCOUNT", "my-agent")
	if got != "my-agent" {
		t.Fatalf("expected ACCOUNT scope identifier to remain unchanged, got %q", got)
	}
}

func TestScopedAgentIdentifier_AccountScopeAlreadyPrefixedUnchanged(t *testing.T) {
	got := scopedAgentIdentifier("ACCOUNT", "account.my-agent")
	if got != "account.my-agent" {
		t.Fatalf("expected existing account-prefixed identifier to remain unchanged, got %q", got)
	}
}

func TestScopedAgentIdentifier_AccountLikeIdentifierWithoutDotUnchanged(t *testing.T) {
	got := scopedAgentIdentifier("ACCOUNT", "accountgitopsagent")
	if got != "accountgitopsagent" {
		t.Fatalf("expected account-like identifier to remain unchanged, got %q", got)
	}
}

func TestProjectIdentifierForAgentScope_ProjectKeepsProjectID(t *testing.T) {
	got := projectIdentifierForAgentScope("PROJECT", "my-project")
	if got != "my-project" {
		t.Fatalf("expected project ID to be kept for PROJECT scope, got %q", got)
	}
}

func TestProjectIdentifierForAgentScope_OrgOmitsProjectID(t *testing.T) {
	got := projectIdentifierForAgentScope("ORG", "my-project")
	if got != "" {
		t.Fatalf("expected project ID to be omitted for ORG scope, got %q", got)
	}
}

func TestProjectIdentifierForAgentScope_AccountOmitsProjectID(t *testing.T) {
	got := projectIdentifierForAgentScope("ACCOUNT", "my-project")
	if got != "" {
		t.Fatalf("expected project ID to be omitted for ACCOUNT scope, got %q", got)
	}
}
