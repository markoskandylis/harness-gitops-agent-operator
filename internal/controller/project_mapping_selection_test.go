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

func TestScopedAgentIdentifier_OrgScopeAddsPrefix(t *testing.T) {
	got := scopedAgentIdentifier("ORG", "my-agent")
	if got != "org.my-agent" {
		t.Fatalf("expected org-prefixed identifier, got %q", got)
	}
}

func TestScopedAgentIdentifier_AlreadyPrefixedUnchanged(t *testing.T) {
	got := scopedAgentIdentifier("ORG", "org.my-agent")
	if got != "org.my-agent" {
		t.Fatalf("expected existing prefixed identifier to remain unchanged, got %q", got)
	}
}

func TestScopedAgentIdentifier_ProjectScopeUnchanged(t *testing.T) {
	got := scopedAgentIdentifier("PROJECT", "my-agent")
	if got != "my-agent" {
		t.Fatalf("expected project scope identifier to remain unchanged, got %q", got)
	}
}
