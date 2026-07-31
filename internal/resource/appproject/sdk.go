package appproject

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/harness/harness-go-sdk/harness/nextgen"

	harnessapi "github.com/markoskandylis/harness-gitops-agent-operator/internal/harness"
)

// ErrAppProjectAlreadyExists reports a create that lost the race: something
// made an AppProject with this name between the reconciler's Get and its
// Create. The reconciler recovers by re-reading and re-evaluating ownership.
//
// There is deliberately NO CreateOutcomeUnknown sentinel here, unlike the
// mapping boundary: a mapping's identity is a server-assigned ID that an
// ambiguous create loses forever, but an AppProject's identity is the
// client-chosen name, so recovery from an ambiguous create is always a
// deterministic Get-by-name on the next reconcile.
var ErrAppProjectAlreadyExists = errors.New("app project already exists")

// AppProjectRequest carries everything the agent-proxied AppProject endpoints
// need. One scope only — the agent's own. An AppProject has no separate
// target scope (that dimension belongs to the Mapping).
type AppProjectRequest struct {
	AccountIdentifier string
	AgentIdentifier   string
	AgentScope        string
	OrgIdentifier     string
	ProjectIdentifier string
	Name              string
}

// AppProject is this package's view of one remote Argo AppProject as observed
// through the agent-proxied API. Identity is the name alone: the API returns
// no Harness scope and no remote ID. UID and ResourceVersion belong to the
// live cluster object behind the agent, usable to cross-check a cluster-side
// read; they are observation-only and are never sent outbound. Only the
// managed spec subset is carried — drift on fields outside it (roles,
// syncWindows, blacklists, ...) is deliberately invisible.
type AppProject struct {
	Name            string
	Namespace       string
	UID             string
	ResourceVersion string

	Description                string
	SourceRepos                []string
	Destinations               []Destination
	ClusterResourceWhitelist   []GroupKind
	NamespaceResourceWhitelist []GroupKind
}

// Destination mirrors one Argo application destination rule.
type Destination struct {
	Server    string
	Namespace string
	Name      string
}

// GroupKind mirrors one Argo group/kind resource-list entry.
type GroupKind struct {
	Group string
	Kind  string
}

// AppProjectLookupResult distinguishes a proven-absent AppProject from a
// failed lookup: absence is a successful answer, not an error.
type AppProjectLookupResult struct {
	Exists     bool
	AppProject AppProject
}

// SDKAppProjectAPI implements the Harness agent-proxied AppProject SDK calls.
type SDKAppProjectAPI struct{}

// Get reads one AppProject through the agent proxy. It calls with the single
// scope-qualified routing identifier instead of looping candidates: on this
// endpoint family a genuine 404 means the AppProject is absent, and a
// candidate fallback would retry an unroutable form and misreport that
// absence as a transient error.
func (SDKAppProjectAPI) Get(
	ctx context.Context,
	session *harnessapi.Session,
	request AppProjectRequest,
) (AppProjectLookupResult, error) {
	candidates := harnessapi.ScopedIdentifierCandidates(request.AgentScope, request.AgentIdentifier)
	if len(candidates) == 0 || strings.TrimSpace(request.Name) == "" {
		return AppProjectLookupResult{}, fmt.Errorf("get AppProject: empty agent identifier or project name")
	}
	routingID := candidates[0]

	response, httpResponse, err := session.Client().ProjectGitOpsApi.AgentProjectServiceGet(
		session.AuthContext(ctx),
		routingID,
		request.Name,
		request.AccountIdentifier,
		&nextgen.ProjectsApiAgentProjectServiceGetOpts{
			OrgIdentifier:     harnessapi.OptionalString(request.OrgIdentifier),
			ProjectIdentifier: harnessapi.OptionalString(request.ProjectIdentifier),
		},
	)
	if err == nil {
		return AppProjectLookupResult{
			Exists:     true,
			AppProject: appProjectFromSDK(response),
		}, nil
	}
	if harnessapi.ClassifyResponse(httpResponse, err) == harnessapi.VerdictAbsent {
		return AppProjectLookupResult{Exists: false}, nil
	}
	return AppProjectLookupResult{}, appProjectAPIError("get AppProject", fmt.Sprintf("appProject=%q", request.Name), httpResponse, err)
}

// List reads every AppProject served by the agent, optionally filtered by
// request.Name (empty lists all). Absence of projects is a 200 with zero
// items; a 404 here can only be about the route or the agent, so every
// error — including NotFound — surfaces as an error, never as an empty list.
func (SDKAppProjectAPI) List(
	ctx context.Context,
	session *harnessapi.Session,
	request AppProjectRequest,
) ([]AppProject, error) {
	candidates := harnessapi.ScopedIdentifierCandidates(request.AgentScope, request.AgentIdentifier)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("list AppProjects: empty agent identifier")
	}
	routingID := candidates[0]

	response, httpResponse, err := session.Client().ProjectGitOpsApi.AgentProjectServiceList(
		session.AuthContext(ctx),
		routingID,
		request.AccountIdentifier,
		&nextgen.ProjectsApiAgentProjectServiceListOpts{
			OrgIdentifier:     harnessapi.OptionalString(request.OrgIdentifier),
			ProjectIdentifier: harnessapi.OptionalString(request.ProjectIdentifier),
			QueryName:         harnessapi.OptionalString(request.Name),
		},
	)
	if err != nil {
		return nil, appProjectAPIError("list AppProjects", fmt.Sprintf("agent=%q", routingID), httpResponse, err)
	}

	projects := make([]AppProject, 0, len(response.Items))
	for _, item := range response.Items {
		projects = append(projects, appProjectFromSDK(item))
	}
	return projects, nil
}

// Create makes one AppProject through the agent proxy. Upsert is always
// false: a name collision must surface as ErrAppProjectAlreadyExists for the
// reconciler's ownership logic, never silently overwrite an object someone
// else made (the UI and Terraform write through this same API). The returned
// object is informational — verification is a Get-after-create, reconciler
// side, never trust-the-create-response.
func (SDKAppProjectAPI) Create(
	ctx context.Context,
	session *harnessapi.Session,
	request AppProjectRequest,
	desired AppProject,
) (AppProject, error) {
	candidates := harnessapi.ScopedIdentifierCandidates(request.AgentScope, request.AgentIdentifier)
	if len(candidates) == 0 || strings.TrimSpace(request.Name) == "" {
		return AppProject{}, fmt.Errorf("create AppProject: empty agent identifier or project name")
	}
	routingID := candidates[0]

	response, httpResponse, err := session.Client().ProjectGitOpsApi.AgentProjectServiceCreate(
		session.AuthContext(ctx),
		nextgen.ProjectsProjectCreateRequest{
			Project: appProjectToSDK(request, desired),
			Upsert:  false,
		},
		request.AccountIdentifier,
		routingID,
		&nextgen.ProjectsApiAgentProjectServiceCreateOpts{
			OrgIdentifier:     harnessapi.OptionalString(request.OrgIdentifier),
			ProjectIdentifier: harnessapi.OptionalString(request.ProjectIdentifier),
		},
	)
	if err == nil {
		return appProjectFromSDK(response), nil
	}
	wrapped := appProjectAPIError("create AppProject", fmt.Sprintf("appProject=%q", request.Name), httpResponse, err)
	if harnessapi.ClassifyResponse(httpResponse, err) == harnessapi.VerdictConflict {
		return AppProject{}, errors.Join(ErrAppProjectAlreadyExists, wrapped)
	}
	return AppProject{}, wrapped
}

// Update replaces the managed spec of one AppProject. The endpoint requires
// the path parameter to equal body.Project.Metadata.Name; both come from
// request.Name here, so the constraint holds by construction.
func (SDKAppProjectAPI) Update(
	ctx context.Context,
	session *harnessapi.Session,
	request AppProjectRequest,
	desired AppProject,
) (AppProject, error) {
	candidates := harnessapi.ScopedIdentifierCandidates(request.AgentScope, request.AgentIdentifier)
	if len(candidates) == 0 || strings.TrimSpace(request.Name) == "" {
		return AppProject{}, fmt.Errorf("update AppProject: empty agent identifier or project name")
	}
	routingID := candidates[0]

	response, httpResponse, err := session.Client().ProjectGitOpsApi.AgentProjectServiceUpdate(
		session.AuthContext(ctx),
		nextgen.ProjectsProjectUpdateRequest{
			Project: appProjectToSDK(request, desired),
		},
		request.AccountIdentifier,
		routingID,
		request.Name,
		&nextgen.ProjectsApiAgentProjectServiceUpdateOpts{
			OrgIdentifier:     harnessapi.OptionalString(request.OrgIdentifier),
			ProjectIdentifier: harnessapi.OptionalString(request.ProjectIdentifier),
		},
	)
	if err != nil {
		return AppProject{}, appProjectAPIError("update AppProject", fmt.Sprintf("appProject=%q", request.Name), httpResponse, err)
	}
	return appProjectFromSDK(response), nil
}

// Delete removes one AppProject through the agent proxy. Absence is success:
// a finalizer re-running after a half-completed cleanup must converge, so an
// already-gone project is a completed delete, not an error. Note the
// generated client sends orgIdentifier as a positional query parameter even
// when empty (ACCOUNT scope) — live behavior for that case is probe-verified
// only for read calls so far.
func (SDKAppProjectAPI) Delete(
	ctx context.Context,
	session *harnessapi.Session,
	request AppProjectRequest,
) error {
	candidates := harnessapi.ScopedIdentifierCandidates(request.AgentScope, request.AgentIdentifier)
	if len(candidates) == 0 || strings.TrimSpace(request.Name) == "" {
		return fmt.Errorf("delete AppProject: empty agent identifier or project name")
	}
	routingID := candidates[0]

	_, httpResponse, err := session.Client().ProjectGitOpsApi.AgentProjectServiceDelete(
		session.AuthContext(ctx),
		routingID,
		request.Name,
		request.AccountIdentifier,
		request.OrgIdentifier,
		&nextgen.ProjectsApiAgentProjectServiceDeleteOpts{
			ProjectIdentifier: harnessapi.OptionalString(request.ProjectIdentifier),
		},
	)
	if err == nil {
		return nil
	}
	if harnessapi.ClassifyResponse(httpResponse, err) == harnessapi.VerdictAbsent {
		return nil
	}
	return appProjectAPIError("delete AppProject", fmt.Sprintf("appProject=%q", request.Name), httpResponse, err)
}

// appProjectAPIError wraps one failed SDK call. Call sites supply the
// resource description because the useful context differs per operation:
// Get/Create/Update/Delete name the AppProject, List names the agent.
func appProjectAPIError(
	operation string,
	resourceDescription string,
	response *http.Response,
	err error,
) error {
	return harnessapi.APIError(operation, resourceDescription, response, err)
}

func appProjectFromSDK(remote nextgen.AppprojectsAppProject) AppProject {
	observed := AppProject{}
	if remote.Metadata != nil {
		observed.Name = remote.Metadata.Name
		observed.Namespace = remote.Metadata.Namespace
		observed.UID = remote.Metadata.Uid
		observed.ResourceVersion = remote.Metadata.ResourceVersion
	}
	if remote.Spec == nil {
		return observed
	}
	observed.Description = remote.Spec.Description
	observed.SourceRepos = append([]string(nil), remote.Spec.SourceRepos...)
	for _, destination := range remote.Spec.Destinations {
		observed.Destinations = append(observed.Destinations, Destination{
			Server:    destination.Server,
			Namespace: destination.Namespace,
			Name:      destination.Name,
		})
	}
	for _, groupKind := range remote.Spec.ClusterResourceWhitelist {
		observed.ClusterResourceWhitelist = append(observed.ClusterResourceWhitelist, GroupKind{
			Group: groupKind.Group,
			Kind:  groupKind.Kind,
		})
	}
	for _, groupKind := range remote.Spec.NamespaceResourceWhitelist {
		observed.NamespaceResourceWhitelist = append(observed.NamespaceResourceWhitelist, GroupKind{
			Group: groupKind.Group,
			Kind:  groupKind.Kind,
		})
	}
	return observed
}

// appProjectToSDK assembles the outbound object for Create/Update. It reads
// ONLY the desired-state half of AppProject: the name is stamped from
// request.Name (identity lives in the request), Namespace is left for the
// agent to decide, and UID/ResourceVersion are observation-only and never
// sent.
func appProjectToSDK(request AppProjectRequest, desired AppProject) *nextgen.AppprojectsAppProject {
	spec := &nextgen.AppprojectsAppProjectSpec{
		Description: desired.Description,
		SourceRepos: append([]string(nil), desired.SourceRepos...),
	}
	for _, destination := range desired.Destinations {
		spec.Destinations = append(spec.Destinations, nextgen.AppprojectsApplicationDestination{
			Server:    destination.Server,
			Namespace: destination.Namespace,
			Name:      destination.Name,
		})
	}
	for _, groupKind := range desired.ClusterResourceWhitelist {
		spec.ClusterResourceWhitelist = append(spec.ClusterResourceWhitelist, nextgen.V1GroupKind{
			Group: groupKind.Group,
			Kind:  groupKind.Kind,
		})
	}
	for _, groupKind := range desired.NamespaceResourceWhitelist {
		spec.NamespaceResourceWhitelist = append(spec.NamespaceResourceWhitelist, nextgen.V1GroupKind{
			Group: groupKind.Group,
			Kind:  groupKind.Kind,
		})
	}
	return &nextgen.AppprojectsAppProject{
		Metadata: &nextgen.V1ObjectMeta{Name: request.Name},
		Spec:     spec,
	}
}
