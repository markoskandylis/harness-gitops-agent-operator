package harness

import (
	"strings"

	"github.com/antihax/optional"
)

func optionalString(value string) optional.String {
	value = strings.TrimSpace(value)
	if value == "" {
		return optional.EmptyString()
	}
	return optional.NewString(value)
}

// OrgIdentifierForAgentScope returns an organization only for ORG- and
// PROJECT-scoped Agent APIs. ACCOUNT-scoped calls must omit it.
func OrgIdentifierForAgentScope(scope string, orgIdentifier string) string {
	if strings.EqualFold(strings.TrimSpace(scope), "ORG") ||
		strings.EqualFold(strings.TrimSpace(scope), "PROJECT") {
		return strings.TrimSpace(orgIdentifier)
	}
	return ""
}

func optionalOrgIdentifier(scope string, orgIdentifier string) optional.String {
	return optionalString(OrgIdentifierForAgentScope(scope, orgIdentifier))
}

// ProjectIdentifierForAgentScope returns a project only for PROJECT-scoped
// agent APIs. ORG and ACCOUNT agent calls must omit it.
func ProjectIdentifierForAgentScope(scope string, projectIdentifier string) string {
	if strings.EqualFold(strings.TrimSpace(scope), "PROJECT") {
		return strings.TrimSpace(projectIdentifier)
	}
	return ""
}

func optionalProjectIdentifier(scope string, projectIdentifier string) optional.String {
	return optionalString(ProjectIdentifierForAgentScope(scope, projectIdentifier))
}

// ScopedPathAgentIdentifierCandidates returns the identifier variants accepted
// by Harness path-based agent endpoints, in preferred order.
func ScopedPathAgentIdentifierCandidates(scope string, identifier string) []string {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil
	}

	candidates := make([]string, 0, 2)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range candidates {
			if existing == value {
				return
			}
		}
		candidates = append(candidates, value)
	}

	if strings.Contains(identifier, ".") {
		add(identifier)
		parts := strings.SplitN(identifier, ".", 2)
		if len(parts) == 2 {
			add(parts[1])
		}
		return candidates
	}

	switch {
	case strings.EqualFold(scope, "ORG"):
		add("org." + identifier)
	case strings.EqualFold(scope, "ACCOUNT"):
		add("account." + identifier)
	}
	add(identifier)
	return candidates
}

// AgentIdentifiersEquivalent compares raw and scope-prefixed agent identifiers.
func AgentIdentifiersEquivalent(scope string, left string, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	for _, candidate := range ScopedPathAgentIdentifierCandidates(scope, right) {
		if left == candidate {
			return true
		}
	}
	return false
}
