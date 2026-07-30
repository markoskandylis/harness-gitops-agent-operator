package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/antihax/optional"
	"github.com/harness/harness-go-sdk/harness/nextgen"
)

var (
	ErrAgentAlreadyExists        = errors.New("harness GitOps agent already exists")
	ErrAgentNotFound             = errors.New("harness GitOps agent not found")
	ErrAgentCreateOutcomeUnknown = errors.New("harness GitOps agent create outcome is unknown")
)

// Agent identifies one Harness GitOps agent and its API scope.
type Agent struct {
	Identifier        string
	Name              string
	AccountIdentifier string
	OrgIdentifier     string
	ProjectIdentifier string
	Scope             string
	Type              string
	Operator          string
	Tags              map[string]string
}

// CreateAgentRequest contains the additional fields used during registration.
type CreateAgentRequest struct {
	Agent
	Namespace string
}

// CreateAgentResult is the identity and initial credential returned by Harness.
type CreateAgentResult struct {
	Identifier         string
	PrefixedIdentifier string
	InitialToken       string
}

// AgentLookupResult is the observed result of an exact raw-identifier lookup.
type AgentLookupResult struct {
	Exists bool
	Agent  Agent
}

// AgentReadiness is the externally observed health of one Harness agent.
type AgentReadiness struct {
	Exists  bool
	Ready   bool
	Message string
}

// SDKAgentAPI implements the Harness agent SDK calls.
type SDKAgentAPI struct{}

func (SDKAgentAPI) Create(
	ctx context.Context,
	session *Session,
	request CreateAgentRequest,
) (CreateAgentResult, error) {
	agentType := nextgen.V1AgentType(request.Type)
	agentScope := nextgen.V1AgentScope(request.Scope)
	agentOperator := nextgen.V1AgentOperator(request.Operator)
	response, httpResponse, err := session.client.AgentApi.AgentServiceForServerCreate(
		session.authContext(ctx),
		nextgen.V1Agent{
			Name:              request.Name,
			Identifier:        request.Identifier,
			Operator:          &agentOperator,
			AccountIdentifier: request.AccountIdentifier,
			OrgIdentifier:     OrgIdentifierForAgentScope(request.Scope, request.OrgIdentifier),
			ProjectIdentifier: ProjectIdentifierForAgentScope(request.Scope, request.ProjectIdentifier),
			Scope:             &agentScope,
			Type_:             &agentType,
			Tags:              cloneTags(request.Tags),
			Metadata: &nextgen.V1AgentMetadata{
				Namespace:        request.Namespace,
				HighAvailability: false,
			},
		},
	)
	if err != nil {
		if isAgentAlreadyExists(httpResponse, err) {
			return CreateAgentResult{}, fmt.Errorf("%w: %q", ErrAgentAlreadyExists, strings.TrimSpace(request.Identifier))
		}
		if isAmbiguousCreateResponse(httpResponse) {
			return CreateAgentResult{}, fmt.Errorf("%w: %w", ErrAgentCreateOutcomeUnknown, err)
		}
		return CreateAgentResult{}, safeAPIError(
			"create GitOps agent",
			request.Identifier,
			httpResponse,
			err,
		)
	}

	result := CreateAgentResult{
		Identifier:         strings.TrimSpace(response.Identifier),
		PrefixedIdentifier: strings.TrimSpace(response.PrefixedIdentifier),
	}
	if result.Identifier == "" {
		return CreateAgentResult{}, fmt.Errorf(
			"%w: Harness returned a successful response without an identifier",
			ErrAgentCreateOutcomeUnknown,
		)
	}
	if response.Credentials != nil {
		result.InitialToken = response.Credentials.PrivateKey
	}
	return result, nil
}

// Lookup gets one Agent without applying name, type, or tag filters: those
// fields are returned and verified by the ownership state machine. Harness
// requires scoped path prefixes for some ACCOUNT/ORG Agents, so a raw fallback
// is attempted only after a canonical candidate returns 404.
func (SDKAgentAPI) Lookup(
	ctx context.Context,
	session *Session,
	agent Agent,
) (AgentLookupResult, error) {
	candidates := ScopedPathAgentIdentifierCandidates(agent.Scope, agent.Identifier)
	if len(candidates) == 0 {
		return AgentLookupResult{}, fmt.Errorf("get GitOps agent: empty agent identifier")
	}

	for _, candidate := range candidates {
		response, httpResponse, err := session.client.AgentApi.AgentServiceForServerGet(
			session.authContext(ctx),
			candidate,
			strings.TrimSpace(agent.AccountIdentifier),
			&nextgen.AgentsApiAgentServiceForServerGetOpts{
				OrgIdentifier:     optionalOrgIdentifier(agent.Scope, agent.OrgIdentifier),
				ProjectIdentifier: optionalProjectIdentifier(agent.Scope, agent.ProjectIdentifier),
				Scope:             optionalString(agent.Scope),
				WithCredentials:   optional.NewBool(false),
			},
		)
		if err == nil {
			return AgentLookupResult{
				Exists: true,
				Agent:  agentFromSDK(response),
			}, nil
		}
		if isNotFound(httpResponse, err) {
			continue
		}
		return AgentLookupResult{}, safeAPIError(
			"get GitOps agent",
			candidate,
			httpResponse,
			err,
		)
	}
	return AgentLookupResult{}, nil
}

func (SDKAgentAPI) Delete(
	ctx context.Context,
	session *Session,
	agent Agent,
) error {
	candidates := ScopedPathAgentIdentifierCandidates(agent.Scope, agent.Identifier)
	if len(candidates) == 0 {
		return fmt.Errorf("delete GitOps agent: empty agent identifier")
	}

	for _, candidate := range candidates {
		_, httpResponse, err := session.client.AgentApi.AgentServiceForServerDelete(
			session.authContext(ctx),
			candidate,
			&nextgen.AgentsApiAgentServiceForServerDeleteOpts{
				AccountIdentifier: optional.NewString(agent.AccountIdentifier),
				OrgIdentifier:     optionalOrgIdentifier(agent.Scope, agent.OrgIdentifier),
				ProjectIdentifier: optionalProjectIdentifier(agent.Scope, agent.ProjectIdentifier),
				Name:              optionalString(agent.Name),
				Type_:             optionalString(agent.Type),
				Scope:             optionalString(agent.Scope),
			},
		)
		if err == nil {
			return nil
		}
		if isNotFound(httpResponse, err) {
			continue
		}
		return safeAPIError("delete GitOps agent", candidate, httpResponse, err)
	}
	return fmt.Errorf("%w: %s", ErrAgentNotFound, strings.TrimSpace(agent.Identifier))
}

func (SDKAgentAPI) ResolveToken(
	ctx context.Context,
	session *Session,
	agent Agent,
	initialToken string,
) (string, error) {
	// A successful create response already returned the one-time credential.
	// Use it directly instead of depending on an immediately consistent GET.
	if initialToken != "" {
		return initialToken, nil
	}

	candidates := ScopedPathAgentIdentifierCandidates(agent.Scope, agent.Identifier)
	if len(candidates) == 0 {
		return "", fmt.Errorf("get GitOps agent credentials: empty agent identifier")
	}

	var response nextgen.V1Agent
	successfulIdentifier := ""
	for _, candidate := range candidates {
		observed, httpResponse, err := session.client.AgentApi.AgentServiceForServerGet(
			session.authContext(ctx),
			candidate,
			agent.AccountIdentifier,
			&nextgen.AgentsApiAgentServiceForServerGetOpts{
				OrgIdentifier:     optionalOrgIdentifier(agent.Scope, agent.OrgIdentifier),
				ProjectIdentifier: optionalProjectIdentifier(agent.Scope, agent.ProjectIdentifier),
				Scope:             optionalString(agent.Scope),
				WithCredentials:   optional.NewBool(true),
			},
		)
		if err == nil {
			response = observed
			successfulIdentifier = candidate
			break
		}
		if isNotFound(httpResponse, err) {
			continue
		}
		return "", safeAPIError("get GitOps agent credentials", candidate, httpResponse, err)
	}
	if successfulIdentifier == "" {
		return "", fmt.Errorf("%w: %s", ErrAgentNotFound, strings.TrimSpace(agent.Identifier))
	}
	if response.Credentials != nil && response.Credentials.PrivateKey != "" {
		return response.Credentials.PrivateKey, nil
	}

	regenerated, _, err := session.client.AgentApi.AgentServiceForServerRegenerateCredentials(
		session.authContext(ctx),
		successfulIdentifier,
	)
	if err != nil {
		return "", WrapAPIError(
			fmt.Sprintf("regenerate credentials for agent %q failed", agent.Identifier),
			err,
		)
	}
	if regenerated.Credentials == nil || regenerated.Credentials.PrivateKey == "" {
		return "", fmt.Errorf("harness API did not return private key for agent %q", agent.Identifier)
	}
	return regenerated.Credentials.PrivateKey, nil
}

func (SDKAgentAPI) Readiness(
	ctx context.Context,
	session *Session,
	agent Agent,
) (AgentReadiness, error) {
	candidates := ScopedPathAgentIdentifierCandidates(agent.Scope, agent.Identifier)
	if len(candidates) == 0 {
		return AgentReadiness{}, fmt.Errorf("get GitOps agent: empty agent identifier")
	}

	for _, candidate := range candidates {
		response, httpResponse, err := session.client.AgentApi.AgentServiceForServerGet(
			session.authContext(ctx),
			candidate,
			agent.AccountIdentifier,
			&nextgen.AgentsApiAgentServiceForServerGetOpts{
				OrgIdentifier:     optionalOrgIdentifier(agent.Scope, agent.OrgIdentifier),
				ProjectIdentifier: optionalProjectIdentifier(agent.Scope, agent.ProjectIdentifier),
				Scope:             optionalString(agent.Scope),
				WithCredentials:   optional.NewBool(false),
			},
		)
		if err == nil {
			return readinessFromAgent(response), nil
		}
		if isNotFound(httpResponse, err) {
			continue
		}
		return AgentReadiness{}, safeAPIError("get GitOps agent", candidate, httpResponse, err)
	}
	return AgentReadiness{}, nil
}

func agentFromSDK(agent nextgen.V1Agent) Agent {
	result := Agent{
		Identifier:        strings.TrimSpace(agent.Identifier),
		Name:              strings.TrimSpace(agent.Name),
		AccountIdentifier: strings.TrimSpace(agent.AccountIdentifier),
		OrgIdentifier:     strings.TrimSpace(agent.OrgIdentifier),
		ProjectIdentifier: strings.TrimSpace(agent.ProjectIdentifier),
		Tags:              cloneTags(agent.Tags),
	}
	if agent.Scope != nil {
		result.Scope = strings.TrimSpace(string(*agent.Scope))
	}
	if agent.Type_ != nil {
		result.Type = strings.TrimSpace(string(*agent.Type_))
	}
	if agent.Operator != nil {
		result.Operator = strings.TrimSpace(string(*agent.Operator))
	}
	return result
}

func cloneTags(tags map[string]string) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(tags))
	for key, value := range tags {
		cloned[key] = value
	}
	return cloned
}

func readinessFromAgent(agent nextgen.V1Agent) AgentReadiness {
	readiness := AgentReadiness{Exists: true}
	if agent.Health == nil {
		readiness.Message = "Harness GitOps agent health has not been reported yet"
		return readiness
	}

	connectionStatus := nextgen.CONNECTED_STATUS_UNSET_V1ConnectedStatus
	if agent.Health.ConnectionStatus != nil {
		connectionStatus = *agent.Health.ConnectionStatus
	}
	healthStatus := nextgen.HEALTH_STATUS_UNSET_Servicev1HealthStatus
	healthMessage := ""
	if agent.Health.HarnessGitopsAgent != nil {
		healthMessage = strings.TrimSpace(agent.Health.HarnessGitopsAgent.Message)
		if agent.Health.HarnessGitopsAgent.Status != nil {
			healthStatus = *agent.Health.HarnessGitopsAgent.Status
		}
	}

	readiness.Ready = connectionStatus == nextgen.CONNECTED_V1ConnectedStatus &&
		healthStatus == nextgen.HEALTHY_Servicev1HealthStatus
	if readiness.Ready {
		readiness.Message = "Harness GitOps agent is Connected and Healthy"
		return readiness
	}

	readiness.Message = fmt.Sprintf(
		"Harness GitOps agent is not ready: connection=%s health=%s",
		connectionStatus,
		healthStatus,
	)
	if healthMessage != "" {
		readiness.Message += ": " + healthMessage
	}
	return readiness
}
