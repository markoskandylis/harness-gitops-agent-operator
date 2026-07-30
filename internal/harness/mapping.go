package harness

import (
	"context"
	"errors"
	"fmt"

	"github.com/antihax/optional"
	"github.com/harness/harness-go-sdk/harness/nextgen"
)

var (
	ErrProjectMappingAlreadyExists        = errors.New("app project mapping already exists")
	ErrProjectMappingCreateOutcomeUnknown = errors.New("app project mapping create outcome is unknown")
)

// Scope identifies a Harness organization/project pair.
type Scope struct {
	OrgIdentifier     string
	ProjectIdentifier string
}

// ProjectMappingRequest keeps the agent lookup scope separate from the mapped
// project scope. This distinction is required for ACCOUNT-scoped agents.
type ProjectMappingRequest struct {
	AccountIdentifier    string
	AgentIdentifier      string
	AgentScope           string
	Agent                Scope
	Mapping              Scope
	ArgoProjectName      string
	AutoCreateServiceEnv bool
}

// ProjectMapping is the stable subset of a Harness AppProject mapping used by
// controller reconciliation and status.
type ProjectMapping struct {
	Identifier           string
	AgentIdentifier      string
	AccountIdentifier    string
	OrgIdentifier        string
	ProjectIdentifier    string
	ArgoProjectName      string
	AutoCreateServiceEnv bool
}

// SDKProjectMappingAPI implements the Harness project-mapping SDK calls.
type SDKProjectMappingAPI struct{}

func (SDKProjectMappingAPI) List(
	ctx context.Context,
	session *Session,
	request ProjectMappingRequest,
) ([]ProjectMapping, error) {
	candidates := ScopedPathAgentIdentifierCandidates(request.AgentScope, request.AgentIdentifier)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("list AppProject mappings: empty agent identifier")
	}

	var lastErr error
	for _, candidate := range candidates {
		response, httpResponse, err := session.client.ProjectMappingsApi.
			AppProjectMappingServiceGetAppProjectMappingsListByAgentV2(
				session.authContext(ctx),
				candidate,
				&nextgen.ProjectMappingsApiAppProjectMappingServiceGetAppProjectMappingsListByAgentV2Opts{
					AccountIdentifier: optional.NewString(request.AccountIdentifier),
					OrgIdentifier:     optionalString(request.Agent.OrgIdentifier),
					ProjectIdentifier: optionalString(request.Agent.ProjectIdentifier),
					ArgoProjectName:   optional.NewString(request.ArgoProjectName),
				},
			)
		if err == nil {
			mappings := make([]ProjectMapping, 0, len(response.AppProjectMappings))
			for _, mapping := range response.AppProjectMappings {
				mappings = append(mappings, projectMappingFromSDK(mapping))
			}
			return mappings, nil
		}
		lastErr = safeAPIError("list AppProject mappings", candidate, httpResponse, err)
		if !isNotFound(httpResponse, err) {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func (SDKProjectMappingAPI) Create(
	ctx context.Context,
	session *Session,
	request ProjectMappingRequest,
) (ProjectMapping, error) {
	candidates := ScopedPathAgentIdentifierCandidates(request.AgentScope, request.AgentIdentifier)
	if len(candidates) == 0 {
		return ProjectMapping{}, fmt.Errorf("create AppProject mapping: empty agent identifier")
	}

	var lastErr error
	for _, candidate := range candidates {
		response, httpResponse, err := session.client.ProjectMappingsApi.AppProjectMappingServiceCreateV2(
			session.authContext(ctx),
			nextgen.V1AppProjectMappingCreateRequestV2{
				AgentIdentifier:      candidate,
				AccountIdentifier:    request.AccountIdentifier,
				OrgIdentifier:        request.Mapping.OrgIdentifier,
				ProjectIdentifier:    request.Mapping.ProjectIdentifier,
				ArgoProjectName:      request.ArgoProjectName,
				AutoCreateServiceEnv: request.AutoCreateServiceEnv,
			},
			candidate,
		)
		if err == nil {
			mapping := projectMappingFromSDK(response)
			if mapping.Identifier == "" {
				return ProjectMapping{}, fmt.Errorf(
					"%w: Harness returned a successful response without an identifier",
					ErrProjectMappingCreateOutcomeUnknown,
				)
			}
			if mapping.AgentIdentifier == "" {
				mapping.AgentIdentifier = candidate
			}
			return mapping, nil
		}
		if isMappingAlreadyExists(httpResponse, err) {
			return ProjectMapping{}, ErrProjectMappingAlreadyExists
		}
		if isAmbiguousCreateResponse(httpResponse) {
			return ProjectMapping{}, fmt.Errorf("%w: %w", ErrProjectMappingCreateOutcomeUnknown, err)
		}
		lastErr = safeAPIError("create AppProject mapping", candidate, httpResponse, err)
		if !isNotFound(httpResponse, err) {
			return ProjectMapping{}, lastErr
		}
	}
	return ProjectMapping{}, lastErr
}

func (SDKProjectMappingAPI) Delete(
	ctx context.Context,
	session *Session,
	request ProjectMappingRequest,
	mappingID string,
) error {
	candidates := ScopedPathAgentIdentifierCandidates(request.AgentScope, request.AgentIdentifier)
	if len(candidates) == 0 {
		return fmt.Errorf("delete AppProject mapping: empty agent identifier")
	}

	for _, candidate := range candidates {
		_, httpResponse, err := session.client.ProjectMappingsApi.AppProjectMappingServiceDeleteV2(
			session.authContext(ctx),
			candidate,
			mappingID,
			&nextgen.ProjectMappingsApiAppProjectMappingServiceDeleteV2Opts{
				AccountIdentifier: optional.NewString(request.AccountIdentifier),
				OrgIdentifier:     optionalString(request.Mapping.OrgIdentifier),
				ProjectIdentifier: optionalString(request.Mapping.ProjectIdentifier),
			},
		)
		if err == nil {
			return nil
		}
		if isNotFound(httpResponse, err) {
			continue
		}
		return safeAPIError("delete AppProject mapping", candidate, httpResponse, err)
	}
	return nil
}

func projectMappingFromSDK(mapping nextgen.V1AppProjectMappingV2) ProjectMapping {
	return ProjectMapping{
		Identifier:           mapping.Identifier,
		AgentIdentifier:      mapping.AgentIdentifier,
		AccountIdentifier:    mapping.AccountIdentifier,
		OrgIdentifier:        mapping.OrgIdentifier,
		ProjectIdentifier:    mapping.ProjectIdentifier,
		ArgoProjectName:      mapping.ArgoProjectName,
		AutoCreateServiceEnv: mapping.AutoCreateServiceEnv,
	}
}
