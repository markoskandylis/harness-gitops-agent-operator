package projectmapping

import (
	"fmt"
	"strings"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

const (
	agentScopeAccount = "ACCOUNT"
	agentScopeOrg     = "ORG"
	agentScopeProject = "PROJECT"
)

// resolveProjectMappingRequest resolves the Agent lookup scope and mapped
// project scope without contacting Harness.
func resolveProjectMappingRequest(
	agent *infrastructurev1.HarnessGitopsAgent,
	mapping *infrastructurev1.HarnessGitopsProjectMapping,
) (ProjectMappingRequest, error) {
	if agent == nil {
		return ProjectMappingRequest{}, fmt.Errorf("referenced HarnessGitopsAgent is nil")
	}
	if mapping == nil {
		return ProjectMappingRequest{}, fmt.Errorf("harnessGitopsProjectMapping is nil")
	}
	if agent.Namespace != mapping.Namespace {
		return ProjectMappingRequest{}, fmt.Errorf(
			"spec.agentRef must reference an Agent in the same namespace %q",
			mapping.Namespace,
		)
	}
	if strings.TrimSpace(mapping.Spec.AgentRef.Name) != agent.Name {
		return ProjectMappingRequest{}, fmt.Errorf(
			"spec.agentRef.name %q does not match Agent %q",
			mapping.Spec.AgentRef.Name,
			agent.Name,
		)
	}

	agentIdentifier := strings.TrimSpace(agent.Spec.ExistingAgentIdentifier)
	if agentIdentifier == "" {
		agentIdentifier = strings.TrimSpace(agent.Status.AgentIdentifier)
	}
	if agentIdentifier == "" {
		return ProjectMappingRequest{}, fmt.Errorf(
			"agent %s/%s is not registered: status.agentIdentifier is empty",
			agent.Namespace,
			agent.Name,
		)
	}

	accountID := strings.TrimSpace(agent.Spec.AccountId)
	if accountID == "" {
		return ProjectMappingRequest{}, fmt.Errorf(
			"agent %s/%s has an empty spec.accountId",
			agent.Namespace,
			agent.Name,
		)
	}

	appProject := strings.TrimSpace(mapping.Spec.AppProject)
	if appProject == "" {
		return ProjectMappingRequest{}, fmt.Errorf("spec.appProject must not be empty")
	}

	scope := strings.ToUpper(strings.TrimSpace(agent.Spec.Scope))
	agentOrgID := strings.TrimSpace(agent.Spec.OrgId)
	agentProjectID := strings.TrimSpace(agent.Spec.ProjectId)
	targetOrgID := strings.TrimSpace(mapping.Spec.OrgID)
	targetProjectID := strings.TrimSpace(mapping.Spec.ProjectID)

	request := ProjectMappingRequest{
		AccountIdentifier:    accountID,
		AgentIdentifier:      agentIdentifier,
		AgentScope:           scope,
		ArgoProjectName:      appProject,
		AutoCreateServiceEnv: mapping.Spec.AutoCreateServiceEnv,
	}

	switch scope {
	case agentScopeProject:
		if agentOrgID == "" || agentProjectID == "" {
			return ProjectMappingRequest{}, fmt.Errorf(
				"PROJECT-scoped Agent requires spec.orgId and spec.projectId",
			)
		}
		if targetOrgID != "" && targetOrgID != agentOrgID {
			return ProjectMappingRequest{}, fmt.Errorf(
				"spec.orgId %q must be empty or match the PROJECT-scoped Agent org %q",
				targetOrgID,
				agentOrgID,
			)
		}
		if targetProjectID != "" && targetProjectID != agentProjectID {
			return ProjectMappingRequest{}, fmt.Errorf(
				"spec.projectId %q must be empty or match the PROJECT-scoped Agent project %q",
				targetProjectID,
				agentProjectID,
			)
		}
		request.Agent = Scope{
			OrgIdentifier:     agentOrgID,
			ProjectIdentifier: agentProjectID,
		}
		request.Mapping = request.Agent

	case agentScopeOrg:
		if agentOrgID == "" {
			return ProjectMappingRequest{}, fmt.Errorf(
				"ORG-scoped Agent requires spec.orgId",
			)
		}
		if targetOrgID != "" && targetOrgID != agentOrgID {
			return ProjectMappingRequest{}, fmt.Errorf(
				"spec.orgId %q must be empty or match the ORG-scoped Agent org %q",
				targetOrgID,
				agentOrgID,
			)
		}
		if targetProjectID == "" {
			return ProjectMappingRequest{}, fmt.Errorf(
				"spec.projectId is required for an ORG-scoped Agent",
			)
		}
		request.Agent = Scope{OrgIdentifier: agentOrgID}
		request.Mapping = Scope{
			OrgIdentifier:     agentOrgID,
			ProjectIdentifier: targetProjectID,
		}

	case agentScopeAccount:
		if targetOrgID == "" || targetProjectID == "" {
			return ProjectMappingRequest{}, fmt.Errorf(
				"spec.orgId and spec.projectId are required for an ACCOUNT-scoped Agent",
			)
		}
		request.Mapping = Scope{
			OrgIdentifier:     targetOrgID,
			ProjectIdentifier: targetProjectID,
		}

	default:
		return ProjectMappingRequest{}, fmt.Errorf(
			"unsupported Agent scope %q: expected ACCOUNT, ORG, or PROJECT",
			agent.Spec.Scope,
		)
	}

	// Keep the canonical identifier in the request. The Harness SDK boundary
	// supplies scope-prefixed path candidates where an endpoint requires them.
	return request, nil
}
