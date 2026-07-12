package controller

import (
	"context"
	stderrors "errors"
	"fmt"
	"sort"
	"strings"

	"github.com/antihax/optional"
	"github.com/harness/harness-go-sdk/harness/nextgen"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

var (
	errArgoProjectMappingNotFound = stderrors.New("argo project mapping not found")
	errArgoProjectScopeMismatch   = stderrors.New("argo project mapping scope mismatch")
)

// deleteAppProjectMapping removes a mapping created for an existing agent.
func (r *HarnessGitopsAgentReconciler) deleteAppProjectMapping(
	session *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	existingAgentIdentifier string,
	mappingID string,
	mappingProjectID string,
) error {
	_, _, err := session.Client.ProjectMappingsApi.AppProjectMappingServiceDeleteV2(
		session.AuthCtx,
		scopedAgentIdentifier(agentCR.Spec.Scope, existingAgentIdentifier),
		mappingID,
		&nextgen.ProjectMappingsApiAppProjectMappingServiceDeleteV2Opts{
			AccountIdentifier: optional.NewString(agentCR.Spec.AccountId),
			OrgIdentifier:     optionalStr(agentCR.Spec.OrgId),
			ProjectIdentifier: optionalStr(mappingProjectID),
		},
	)
	if err == nil {
		return nil
	}

	if swaggerErr, ok := err.(nextgen.GenericSwaggerError); ok {
		body := strings.ToLower(string(swaggerErr.Body()))
		if strings.Contains(body, "not found") {
			return nil
		}
	}
	return err
}

// fetchArgoProjectId resolves the Argo AppProject name for an agent by using the
// latest v2 project-mapping endpoint, with a v1 fallback for compatibility.
func (r *HarnessGitopsAgentReconciler) fetchArgoProjectId(
	harnessSession *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
) (string, error) {
	v2Resp, _, v2Err := harnessSession.Client.ProjectMappingsApi.AppProjectMappingServiceGetAppProjectMappingsListByAgentV2(
		harnessSession.AuthCtx,
		agentIdentifier,
		&nextgen.ProjectMappingsApiAppProjectMappingServiceGetAppProjectMappingsListByAgentV2Opts{
			AccountIdentifier: optional.NewString(agentCR.Spec.AccountId),
			OrgIdentifier:     optionalStr(agentCR.Spec.OrgId),
			ProjectIdentifier: optionalStr(agentCR.Spec.ProjectId),
		},
	)
	if v2Err == nil {
		projectID, err := selectArgoProjectIDFromV2Mappings(
			v2Resp.AppProjectMappings,
			agentCR.Spec.AccountId,
			agentCR.Spec.OrgId,
			agentCR.Spec.ProjectId,
		)
		if err == nil {
			return projectID, nil
		}
	}

	v1Resp, _, v1Err := harnessSession.Client.ProjectMappingsApi.AppProjectMappingServiceGetAppProjectMappingListByAgent(
		harnessSession.AuthCtx,
		agentIdentifier,
		&nextgen.ProjectMappingsApiAppProjectMappingServiceGetAppProjectMappingListByAgentOpts{
			AccountIdentifier: optional.NewString(agentCR.Spec.AccountId),
			OrgIdentifier:     optionalStr(agentCR.Spec.OrgId),
			ProjectIdentifier: optionalStr(agentCR.Spec.ProjectId),
		},
	)
	if v1Err != nil {
		if v2Err != nil {
			return "", fmt.Errorf("project mappings v2 failed: %w; v1 fallback failed: %v", v2Err, v1Err)
		}
		return "", v1Err
	}

	projectID, selErr := selectArgoProjectIDFromV1Mapping(v1Resp.AppProjMap, agentCR.Spec.OrgId, agentCR.Spec.ProjectId)
	if selErr != nil {
		if v2Err != nil {
			return "", fmt.Errorf("project mappings v2 failed: %w; v1 fallback returned no scoped mapping: %v", v2Err, selErr)
		}
		return "", selErr
	}
	return projectID, nil
}

// createAppProjectMapping calls AppProjectMappingServiceCreateV2 to map an existing in-cluster
// ArgoCD AppProject to a specific Harness project using an already-running agent.
// Returns the mapping Identifier on success, or empty string if the mapping already exists.
func (r *HarnessGitopsAgentReconciler) createAppProjectMapping(
	ctx context.Context,
	session *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
	argoProjectName string,
	projectID string,
) (string, error) {
	_ = ctx
	candidates := scopedPathAgentIdentifierCandidates(agentCR.Spec.Scope, agentIdentifier)
	if len(candidates) == 0 {
		return "", fmt.Errorf("createAppProjectMapping failed: empty agent identifier")
	}

	var lastErr error
	for _, candidate := range candidates {
		resp, _, err := session.Client.ProjectMappingsApi.AppProjectMappingServiceCreateV2(
			session.AuthCtx,
			nextgen.V1AppProjectMappingCreateRequestV2{
				AgentIdentifier:   candidate,
				AccountIdentifier: agentCR.Spec.AccountId,
				OrgIdentifier:     agentCR.Spec.OrgId,
				ProjectIdentifier: projectID,
				ArgoProjectName:   argoProjectName,
			},
			candidate,
		)
		if err == nil {
			return resp.Identifier, nil
		}
		if swaggerErr, ok := err.(nextgen.GenericSwaggerError); ok {
			body := strings.ToLower(string(swaggerErr.Body()))
			if strings.Contains(body, "already exists") {
				return "", nil
			}
		}
		lastErr = fmt.Errorf(
			"createAppProjectMapping failed for agentIdentifier=%q: %s",
			candidate,
			harnessAPIErrorDetails(err),
		)
	}
	return "", lastErr
}

func selectArgoProjectIDFromV2Mappings(
	mappings []nextgen.V1AppProjectMappingV2,
	accountID string,
	orgID string,
	projectID string,
) (string, error) {
	if len(mappings) == 0 {
		return "", fmt.Errorf("%w: v2 returned no mappings", errArgoProjectMappingNotFound)
	}

	candidateSet := map[string]struct{}{}
	scopeMismatch := false
	for _, mapping := range mappings {
		if mapping.AccountIdentifier == accountID &&
			mapping.OrgIdentifier == orgID &&
			mapping.ProjectIdentifier == projectID {
			name := strings.TrimSpace(mapping.ArgoProjectName)
			if name != "" {
				candidateSet[name] = struct{}{}
			}
			continue
		}
		scopeMismatch = true
	}

	if len(candidateSet) == 0 {
		if scopeMismatch {
			return "", fmt.Errorf("%w: expected account=%s org=%s project=%s", errArgoProjectScopeMismatch, accountID, orgID, projectID)
		}
		return "", fmt.Errorf("%w: no usable argoProjectName for account=%s org=%s project=%s", errArgoProjectMappingNotFound, accountID, orgID, projectID)
	}

	candidates := make([]string, 0, len(candidateSet))
	for candidate := range candidateSet {
		candidates = append(candidates, candidate)
	}
	sort.Strings(candidates)
	return candidates[0], nil
}

func selectArgoProjectIDFromV1Mapping(
	appProjMap map[string]nextgen.Servicev1Project,
	orgID string,
	projectID string,
) (string, error) {
	if len(appProjMap) == 0 {
		return "", fmt.Errorf("%w: v1 returned empty appProjMap", errArgoProjectMappingNotFound)
	}

	candidateSet := map[string]struct{}{}
	scopeMismatch := false
	for argoProjectID, project := range appProjMap {
		if project.OrgIdentifier == orgID && project.ProjectIdentifier == projectID {
			if strings.TrimSpace(argoProjectID) != "" {
				candidateSet[argoProjectID] = struct{}{}
			}
			continue
		}
		scopeMismatch = true
	}

	if len(candidateSet) == 0 {
		if scopeMismatch {
			return "", fmt.Errorf("%w: expected org=%s project=%s", errArgoProjectScopeMismatch, orgID, projectID)
		}
		return "", fmt.Errorf("%w: no scoped v1 app project mapping for org=%s project=%s", errArgoProjectMappingNotFound, orgID, projectID)
	}

	candidates := make([]string, 0, len(candidateSet))
	for candidate := range candidateSet {
		candidates = append(candidates, candidate)
	}
	sort.Strings(candidates)
	return candidates[0], nil
}
