package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/antihax/optional"
	"github.com/harness/harness-go-sdk/harness/nextgen"

	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
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
	session *harnessapi.Session,
	request CreateAgentRequest,
) (CreateAgentResult, error) {
	agentType := nextgen.V1AgentType(request.Type)
	agentScope := nextgen.V1AgentScope(request.Scope)
	agentOperator := nextgen.V1AgentOperator(request.Operator)
	response, httpResponse, err := session.Client().AgentApi.AgentServiceForServerCreate(
		session.AuthContext(ctx),
		nextgen.V1Agent{
			Name:              request.Name,
			Identifier:        request.Identifier,
			Operator:          &agentOperator,
			AccountIdentifier: request.AccountIdentifier,
			OrgIdentifier:     harnessapi.OrgIdentifierForScope(request.Scope, request.OrgIdentifier),
			ProjectIdentifier: harnessapi.ProjectIdentifierForScope(request.Scope, request.ProjectIdentifier),
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
		if harnessapi.ClassifyResponse(httpResponse, err) == harnessapi.VerdictTransient {
			return CreateAgentResult{}, fmt.Errorf("%w: %w", ErrAgentCreateOutcomeUnknown, err)
		}
		return CreateAgentResult{}, agentAPIError(
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
	session *harnessapi.Session,
	agent Agent,
) (AgentLookupResult, error) {
	candidates := harnessapi.ScopedIdentifierCandidates(agent.Scope, agent.Identifier)
	if len(candidates) == 0 {
		return AgentLookupResult{}, fmt.Errorf("get GitOps agent: empty agent identifier")
	}

	for _, candidate := range candidates {
		response, httpResponse, err := session.Client().AgentApi.AgentServiceForServerGet(
			session.AuthContext(ctx),
			candidate,
			strings.TrimSpace(agent.AccountIdentifier),
			&nextgen.AgentsApiAgentServiceForServerGetOpts{
				OrgIdentifier:     harnessapi.OptionalOrgIdentifier(agent.Scope, agent.OrgIdentifier),
				ProjectIdentifier: harnessapi.OptionalProjectIdentifier(agent.Scope, agent.ProjectIdentifier),
				Scope:             harnessapi.OptionalString(agent.Scope),
				WithCredentials:   optional.NewBool(false),
			},
		)
		if err == nil {
			return AgentLookupResult{
				Exists: true,
				Agent:  agentFromSDK(response),
			}, nil
		}
		if harnessapi.ClassifyResponse(httpResponse, err) == harnessapi.VerdictAbsent {
			continue
		}
		return AgentLookupResult{}, agentAPIError(
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
	session *harnessapi.Session,
	agent Agent,
) error {
	candidates := harnessapi.ScopedIdentifierCandidates(agent.Scope, agent.Identifier)
	if len(candidates) == 0 {
		return fmt.Errorf("delete GitOps agent: empty agent identifier")
	}

	for _, candidate := range candidates {
		_, httpResponse, err := session.Client().AgentApi.AgentServiceForServerDelete(
			session.AuthContext(ctx),
			candidate,
			&nextgen.AgentsApiAgentServiceForServerDeleteOpts{
				AccountIdentifier: optional.NewString(agent.AccountIdentifier),
				OrgIdentifier:     harnessapi.OptionalOrgIdentifier(agent.Scope, agent.OrgIdentifier),
				ProjectIdentifier: harnessapi.OptionalProjectIdentifier(agent.Scope, agent.ProjectIdentifier),
				Name:              harnessapi.OptionalString(agent.Name),
				Type_:             harnessapi.OptionalString(agent.Type),
				Scope:             harnessapi.OptionalString(agent.Scope),
			},
		)
		if err == nil {
			return nil
		}
		if harnessapi.ClassifyResponse(httpResponse, err) == harnessapi.VerdictAbsent {
			continue
		}
		return agentAPIError("delete GitOps agent", candidate, httpResponse, err)
	}
	return fmt.Errorf("%w: %s", ErrAgentNotFound, strings.TrimSpace(agent.Identifier))
}

func (SDKAgentAPI) ResolveToken(
	ctx context.Context,
	session *harnessapi.Session,
	agent Agent,
	initialToken string,
) (string, error) {
	// A successful create response already returned the one-time credential.
	// Use it directly instead of depending on an immediately consistent GET.
	if initialToken != "" {
		return initialToken, nil
	}

	candidates := harnessapi.ScopedIdentifierCandidates(agent.Scope, agent.Identifier)
	if len(candidates) == 0 {
		return "", fmt.Errorf("get GitOps agent credentials: empty agent identifier")
	}

	var response nextgen.V1Agent
	successfulIdentifier := ""
	for _, candidate := range candidates {
		observed, httpResponse, err := session.Client().AgentApi.AgentServiceForServerGet(
			session.AuthContext(ctx),
			candidate,
			agent.AccountIdentifier,
			&nextgen.AgentsApiAgentServiceForServerGetOpts{
				OrgIdentifier:     harnessapi.OptionalOrgIdentifier(agent.Scope, agent.OrgIdentifier),
				ProjectIdentifier: harnessapi.OptionalProjectIdentifier(agent.Scope, agent.ProjectIdentifier),
				Scope:             harnessapi.OptionalString(agent.Scope),
				WithCredentials:   optional.NewBool(true),
			},
		)
		if err == nil {
			response = observed
			successfulIdentifier = candidate
			break
		}
		if harnessapi.ClassifyResponse(httpResponse, err) == harnessapi.VerdictAbsent {
			continue
		}
		return "", agentAPIError("get GitOps agent credentials", candidate, httpResponse, err)
	}
	if successfulIdentifier == "" {
		return "", fmt.Errorf("%w: %s", ErrAgentNotFound, strings.TrimSpace(agent.Identifier))
	}
	if response.Credentials != nil && response.Credentials.PrivateKey != "" {
		return response.Credentials.PrivateKey, nil
	}

	regenerated, httpResponse, err := session.Client().AgentApi.AgentServiceForServerRegenerateCredentials(
		session.AuthContext(ctx),
		successfulIdentifier,
	)
	if err != nil {
		return "", agentAPIError(
			"regenerate credentials for GitOps agent",
			successfulIdentifier,
			httpResponse,
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
	session *harnessapi.Session,
	agent Agent,
) (AgentReadiness, error) {
	candidates := harnessapi.ScopedIdentifierCandidates(agent.Scope, agent.Identifier)
	if len(candidates) == 0 {
		return AgentReadiness{}, fmt.Errorf("get GitOps agent: empty agent identifier")
	}

	for _, candidate := range candidates {
		response, httpResponse, err := session.Client().AgentApi.AgentServiceForServerGet(
			session.AuthContext(ctx),
			candidate,
			agent.AccountIdentifier,
			&nextgen.AgentsApiAgentServiceForServerGetOpts{
				OrgIdentifier:     harnessapi.OptionalOrgIdentifier(agent.Scope, agent.OrgIdentifier),
				ProjectIdentifier: harnessapi.OptionalProjectIdentifier(agent.Scope, agent.ProjectIdentifier),
				Scope:             harnessapi.OptionalString(agent.Scope),
				WithCredentials:   optional.NewBool(false),
			},
		)
		if err == nil {
			return readinessFromAgent(response), nil
		}
		if harnessapi.ClassifyResponse(httpResponse, err) == harnessapi.VerdictAbsent {
			continue
		}
		return AgentReadiness{}, agentAPIError("get GitOps agent", candidate, httpResponse, err)
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

func isAgentAlreadyExists(response *http.Response, err error) bool {
	return harnessapi.ClassifyResponse(response, err) == harnessapi.VerdictConflict ||
		strings.Contains(strings.ToLower(harnessapi.ErrorBody(err)), "agent already exists")
}

func isAgentNotFound(err error) bool {
	return errors.Is(err, ErrAgentNotFound) ||
		harnessapi.VerdictOf(err) == harnessapi.VerdictAbsent
}

func agentAPIError(
	operation string,
	identifier string,
	response *http.Response,
	err error,
) error {
	return harnessapi.APIError(
		operation,
		fmt.Sprintf("agentIdentifier=%q", identifier),
		response,
		err,
	)
}
