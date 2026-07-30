package harness

import (
	"strings"

	"github.com/antihax/optional"
)

// OptionalString omits an empty value from a Harness SDK request.
func OptionalString(value string) optional.String {
	value = strings.TrimSpace(value)
	if value == "" {
		return optional.EmptyString()
	}
	return optional.NewString(value)
}

// OrgIdentifierForScope returns an organization only for ORG- and
// PROJECT-scoped APIs.
func OrgIdentifierForScope(scope string, orgIdentifier string) string {
	if strings.EqualFold(strings.TrimSpace(scope), "ORG") ||
		strings.EqualFold(strings.TrimSpace(scope), "PROJECT") {
		return strings.TrimSpace(orgIdentifier)
	}
	return ""
}

// OptionalOrgIdentifier omits organizations from ACCOUNT-scoped requests.
func OptionalOrgIdentifier(scope string, orgIdentifier string) optional.String {
	return OptionalString(OrgIdentifierForScope(scope, orgIdentifier))
}

// ProjectIdentifierForScope returns a project only for PROJECT-scoped APIs.
func ProjectIdentifierForScope(scope string, projectIdentifier string) string {
	if strings.EqualFold(strings.TrimSpace(scope), "PROJECT") {
		return strings.TrimSpace(projectIdentifier)
	}
	return ""
}

// OptionalProjectIdentifier omits projects from ACCOUNT- and ORG-scoped
// requests.
func OptionalProjectIdentifier(scope string, projectIdentifier string) optional.String {
	return OptionalString(ProjectIdentifierForScope(scope, projectIdentifier))
}

// ScopedIdentifierCandidates returns the identifier variants accepted by
// Harness path-based scoped endpoints, in preferred order.
func ScopedIdentifierCandidates(scope string, identifier string) []string {
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

// IdentifiersEquivalent compares raw and scope-prefixed identifiers.
func IdentifiersEquivalent(scope string, left string, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	for _, candidate := range ScopedIdentifierCandidates(scope, right) {
		if left == candidate {
			return true
		}
	}
	return false
}
