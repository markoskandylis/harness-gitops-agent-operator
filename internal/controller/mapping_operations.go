package controller

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/antihax/optional"
	"github.com/harness/harness-go-sdk/harness/nextgen"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

var (
	errArgoProjectMappingNotFound     = stderrors.New("argo project mapping not found")
	errAppProjectMappingMismatch      = stderrors.New("app project mapping mismatch")
	errAppProjectMappingAlreadyExists = stderrors.New("app project mapping already exists")
	errAppProjectMappingNotVerified   = stderrors.New("app project mapping was not verified")
)

const (
	mappingReadyConditionType = "MappingReady"

	mappingReasonAppProjectNotFound    = "AppProjectNotFound"
	mappingReasonAgentNotFound         = "AgentNotFound"
	mappingReasonAgentNotHealthy       = "AgentNotHealthy"
	mappingReasonMappingCreated        = "MappingCreated"
	mappingReasonMappingVerified       = "MappingVerified"
	mappingReasonMappingMismatch       = "MappingMismatch"
	mappingReasonVerificationFailed    = "MappingVerificationFailed"
	mappingReasonInvalidProjectMapping = "InvalidProjectMapping"
	mappingReasonMappingRemoved        = "MappingRemoved"
	mappingReasonCleanupBlocked        = "MappingCleanupBlocked"

	// DefaultAppProjectPendingRetryInterval bounds initial mapping latency while
	// the AppProject is waiting to be installed by the bootstrap chart.
	DefaultAppProjectPendingRetryInterval = 20 * time.Second
	// DefaultHarnessMappingResyncInterval periodically verifies external state.
	DefaultHarnessMappingResyncInterval = 5 * time.Minute
	// DefaultHarnessHTTPTimeout caps the full round-trip of a single Harness API
	// attempt.
	DefaultHarnessHTTPTimeout = 60 * time.Second
	// MinimumMappingInterval prevents invalid or accidental tight reconcile loops.
	MinimumMappingInterval = time.Second
)

// ValidateMappingIntervals validates production controller flag values.
func ValidateMappingIntervals(appProjectPendingRetry time.Duration, harnessMappingResync time.Duration) error {
	if appProjectPendingRetry < MinimumMappingInterval {
		return fmt.Errorf(
			"AppProject pending retry interval must be at least %s",
			MinimumMappingInterval,
		)
	}
	if harnessMappingResync < MinimumMappingInterval {
		return fmt.Errorf(
			"harness mapping resync interval must be at least %s",
			MinimumMappingInterval,
		)
	}
	return nil
}

func (r *HarnessGitopsAgentReconciler) appProjectPendingRetryInterval() time.Duration {
	if r.AppProjectPendingRetryInterval > 0 {
		return r.AppProjectPendingRetryInterval
	}
	return DefaultAppProjectPendingRetryInterval
}

func (r *HarnessGitopsAgentReconciler) harnessMappingResyncInterval() time.Duration {
	if r.HarnessMappingResyncInterval > 0 {
		return r.HarnessMappingResyncInterval
	}
	return DefaultHarnessMappingResyncInterval
}

type harnessScope struct {
	OrgIdentifier     string
	ProjectIdentifier string
}

type appProjectMappingRequest struct {
	AccountIdentifier string
	AgentIdentifier   string
	AgentScope        string
	// Agent locates the agent being queried.
	Agent harnessScope

	// Mapping describes the Harness project the AppProject is mapped to. The
	// Create body and selectExactAppProjectMapping use this and nothing else.
	Mapping harnessScope

	ArgoProjectName string
}

// appProjectMappingAPI isolates the Harness SDK project-mapping calls
// (List/Create/Delete) behind a seam so tests can substitute a fake.
type appProjectMappingAPI interface {
	List(
		ctx context.Context,
		session *HarnessSession,
		request appProjectMappingRequest,
	) ([]nextgen.V1AppProjectMappingV2, error)
	Create(ctx context.Context, session *HarnessSession, request appProjectMappingRequest) error
	Delete(
		ctx context.Context,
		session *HarnessSession,
		request appProjectMappingRequest,
		mappingID string,
	) error
}

type harnessAgentReadiness struct {
	Exists  bool
	Ready   bool
	Message string
}

type agentReadinessChecker interface {
	Readiness(
		ctx context.Context,
		session *HarnessSession,
		agentCR *infrastructurev1.HarnessGitopsAgent,
		agentIdentifier string,
	) (harnessAgentReadiness, error)
}

type sdkAppProjectMappingAPI struct{}

type sdkAgentReadinessChecker struct{}

func (r *HarnessGitopsAgentReconciler) appProjectMappingAPI() appProjectMappingAPI {
	if r.mappingAPI != nil {
		return r.mappingAPI
	}
	return sdkAppProjectMappingAPI{}
}

func (r *HarnessGitopsAgentReconciler) gitOpsAgentReadinessChecker() agentReadinessChecker {
	if r.agentReadinessChecker != nil {
		return r.agentReadinessChecker
	}
	return sdkAgentReadinessChecker{}
}

// deleteAppProjectMapping removes the mapping this resource owns, for the shared
// (existing agent) path where the agent itself must survive.
//
// It re-Lists rather than trusting Status.ArgoProjectMappingId. A mapping can be
// edited in place (PUT /gitops/api/v2/agents/{agent}/appprojectsmapping/{id}), so
// a remembered ID proves the row still exists, not that it is still ours. Only
// the full tuple proves identity, which is the same guarantee the create path
// relies on.
//
// Absent or mismatched means there is nothing of ours to clean up, so both return
// nil and let the finalizer drop. Refusing to delete is correct; blocking
// deletion over someone else's mapping would strand the resource forever.
func (r *HarnessGitopsAgentReconciler) deleteAppProjectMapping(
	ctx context.Context,
	session *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
	target *projectMappingTarget,
) error {
	if target == nil {
		return nil
	}

	log := logf.FromContext(ctx)
	if agentCR.Status.ArgoProjectMappingOwnership != infrastructurev1.OwnershipManaged {
		log.Info(
			"Skipping AppProject mapping delete because controller ownership is not recorded",
			"appProject", target.AppProject,
			"mappingId", agentCR.Status.ArgoProjectMappingId,
			"mappingOwnership", agentCR.Status.ArgoProjectMappingOwnership,
		)
		return nil
	}
	rememberedID := strings.TrimSpace(agentCR.Status.ArgoProjectMappingId)
	if rememberedID == "" {
		log.Info(
			"Skipping AppProject mapping delete because its managed identifier is not recorded",
			"appProject", target.AppProject,
		)
		return nil
	}

	request := appProjectMappingRequestFor(agentCR, agentIdentifier, target)

	mappings, err := r.appProjectMappingAPI().List(ctx, session, request)
	if err != nil {
		return err
	}

	mapping, err := selectExactAppProjectMapping(mappings, request)
	if err != nil {
		switch {
		case stderrors.Is(err, errArgoProjectMappingNotFound):
			log.Info("AppProject mapping already absent; nothing to delete",
				"appProject", target.AppProject)
			return nil
		case stderrors.Is(err, errAppProjectMappingMismatch):
			log.Info("Refusing to delete an AppProject mapping that does not match this resource",
				"appProject", target.AppProject,
				"org", target.OrgID,
				"project", target.ProjectID)
			return nil
		default:
			return err
		}
	}

	if rememberedID != strings.TrimSpace(mapping.Identifier) {
		log.Info("Refusing to delete an AppProject mapping recreated outside the controller",
			"rememberedId", rememberedID, "liveId", mapping.Identifier)
		return nil
	}

	log.Info("Deleting AppProject mapping", "mappingId", mapping.Identifier)
	return r.appProjectMappingAPI().Delete(ctx, session, request, mapping.Identifier)
}

// appProjectMappingRequestFor is the single place the two Harness scopes are
// derived from a CR. Both the reconcile and the delete path go through it so the
// agent/mapping split can only ever be got right once.
func appProjectMappingRequestFor(
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
	target *projectMappingTarget,
) appProjectMappingRequest {
	return appProjectMappingRequest{
		AccountIdentifier: strings.TrimSpace(agentCR.Spec.AccountId),
		AgentIdentifier:   strings.TrimSpace(agentIdentifier),
		AgentScope:        strings.TrimSpace(agentCR.Spec.Scope),
		Agent: harnessScope{
			OrgIdentifier:     strings.TrimSpace(agentCR.Spec.OrgId),
			ProjectIdentifier: projectIdentifierForAgentScope(agentCR.Spec.Scope, agentCR.Spec.ProjectId),
		},
		Mapping: harnessScope{
			OrgIdentifier:     target.OrgID,
			ProjectIdentifier: target.ProjectID,
		},
		ArgoProjectName: target.AppProject,
	}
}

func managedMappingCleanupTarget(
	agentCR *infrastructurev1.HarnessGitopsAgent,
	desired *projectMappingTarget,
) (*projectMappingTarget, error) {
	if agentCR.Status.ArgoProjectMappingOwnership != infrastructurev1.OwnershipManaged ||
		strings.TrimSpace(agentCR.Status.ArgoProjectMappingId) == "" {
		return nil, nil
	}

	observed := &projectMappingTarget{
		OrgID:      strings.TrimSpace(agentCR.Status.ArgoProjectMappingOrgId),
		ProjectID:  strings.TrimSpace(agentCR.Status.ArgoProjectMappingProjectId),
		AppProject: strings.TrimSpace(agentCR.Status.ArgoProjectId),
	}

	if observed.OrgID == "" || observed.ProjectID == "" || observed.AppProject == "" {
		return nil, fmt.Errorf(
			"cannot clean up managed mapping %q because its resolved org/project/AppProject status is incomplete",
			agentCR.Status.ArgoProjectMappingId,
		)
	}
	if desired != nil &&
		observed.OrgID == desired.OrgID &&
		observed.ProjectID == desired.ProjectID &&
		observed.AppProject == desired.AppProject {
		return nil, nil
	}
	return observed, nil
}

func (r *HarnessGitopsAgentReconciler) reconcileStaleMapping(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	existingAgentIdentifier string,
	existingAgentMode bool,
	desired *projectMappingTarget,
) (ctrl.Result, bool, error) {
	staleTarget, err := managedMappingCleanupTarget(agentCR, desired)
	if err != nil {
		conditionErr := r.setMappingCondition(
			ctx,
			agentCR,
			mappingReasonCleanupBlocked,
			err.Error(),
		)
		return ctrl.Result{}, true, stderrors.Join(err, conditionErr)
	}
	if staleTarget == nil {
		if desired == nil && mappingStateRecorded(agentCR) {
			return ctrl.Result{}, true, r.setMappingRemovedStatus(ctx, agentCR)
		}
		return ctrl.Result{}, false, nil
	}

	harnessSession, err := r.getHarnessClient(ctx, agentCR)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	agentIdentifier := agentIdentifierForStatus(agentCR)
	if existingAgentMode {
		agentIdentifier = existingAgentIdentifier
	}
	if err := r.deleteAppProjectMapping(
		ctx,
		harnessSession,
		agentCR,
		agentIdentifier,
		staleTarget,
	); err != nil {
		return ctrl.Result{}, true, err
	}
	if err := r.setMappingRemovedStatus(ctx, agentCR); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{RequeueAfter: MinimumMappingInterval}, true, nil
}

func mappingStateRecorded(agentCR *infrastructurev1.HarnessGitopsAgent) bool {
	return agentCR.Status.ArgoProjectId != "" ||
		agentCR.Status.ArgoProjectMappingId != "" ||
		agentCR.Status.ArgoProjectMappingOwnership != "" ||
		agentCR.Status.ArgoProjectMappingOrgId != "" ||
		agentCR.Status.ArgoProjectMappingProjectId != "" ||
		apiMeta.FindStatusCondition(agentCR.Status.Conditions, mappingReadyConditionType) != nil
}

func (sdkAppProjectMappingAPI) List(
	ctx context.Context,
	session *HarnessSession,
	request appProjectMappingRequest,
) ([]nextgen.V1AppProjectMappingV2, error) {
	_ = ctx
	candidates := scopedPathAgentIdentifierCandidates(request.AgentScope, request.AgentIdentifier)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("list AppProject mappings: empty agent identifier")
	}

	var lastErr error
	for _, candidate := range candidates {
		response, httpResponse, err := session.Client.ProjectMappingsApi.
			AppProjectMappingServiceGetAppProjectMappingsListByAgentV2(
				session.AuthCtx,
				candidate,
				&nextgen.ProjectMappingsApiAppProjectMappingServiceGetAppProjectMappingsListByAgentV2Opts{
					AccountIdentifier: optional.NewString(request.AccountIdentifier),
					OrgIdentifier:     optionalStr(request.Agent.OrgIdentifier),
					ProjectIdentifier: optionalStr(request.Agent.ProjectIdentifier),
					ArgoProjectName:   optional.NewString(request.ArgoProjectName),
				},
			)
		if err == nil {
			// A successful response is authoritative even when it is empty.
			// Trying an alternate path after a 200 can turn a legitimate empty
			// result into a retried Harness 5xx and block the reconcile worker.
			return response.AppProjectMappings, nil
		}
		lastErr = safeHarnessAPIError("list AppProject mappings", candidate, httpResponse, err)
		if !isHarnessNotFound(httpResponse, err) {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func (sdkAppProjectMappingAPI) Create(
	ctx context.Context,
	session *HarnessSession,
	request appProjectMappingRequest,
) error {
	_ = ctx
	candidates := scopedPathAgentIdentifierCandidates(request.AgentScope, request.AgentIdentifier)
	if len(candidates) == 0 {
		return fmt.Errorf("create AppProject mapping: empty agent identifier")
	}

	var lastErr error
	for _, candidate := range candidates {
		_, httpResponse, err := session.Client.ProjectMappingsApi.AppProjectMappingServiceCreateV2(
			session.AuthCtx,
			nextgen.V1AppProjectMappingCreateRequestV2{
				AgentIdentifier:   candidate,
				AccountIdentifier: request.AccountIdentifier,
				OrgIdentifier:     request.Mapping.OrgIdentifier,
				ProjectIdentifier: request.Mapping.ProjectIdentifier,
				ArgoProjectName:   request.ArgoProjectName,
			},
			candidate,
		)
		if err == nil {
			return nil
		}
		if isAppProjectMappingAlreadyExists(httpResponse, err) {
			return errAppProjectMappingAlreadyExists
		}
		lastErr = safeHarnessAPIError("create AppProject mapping", candidate, httpResponse, err)
	}
	return lastErr
}

func (sdkAppProjectMappingAPI) Delete(
	ctx context.Context,
	session *HarnessSession,
	request appProjectMappingRequest,
	mappingID string,
) error {
	_ = ctx
	candidates := scopedPathAgentIdentifierCandidates(request.AgentScope, request.AgentIdentifier)
	if len(candidates) == 0 {
		return fmt.Errorf("delete AppProject mapping: empty agent identifier")
	}

	for _, candidate := range candidates {
		_, httpResponse, err := session.Client.ProjectMappingsApi.AppProjectMappingServiceDeleteV2(
			session.AuthCtx,
			candidate,
			mappingID,
			&nextgen.ProjectMappingsApiAppProjectMappingServiceDeleteV2Opts{
				AccountIdentifier: optional.NewString(request.AccountIdentifier),
				OrgIdentifier:     optionalStr(request.Mapping.OrgIdentifier),
				ProjectIdentifier: optionalStr(request.Mapping.ProjectIdentifier),
			},
		)
		if err == nil {
			return nil
		}
		// Already gone is the desired end state; try the next identifier shape
		// in case this one was simply the wrong path form.
		if isHarnessNotFound(httpResponse, err) {
			continue
		}
		// A non-404 is authoritative and must not be retried on an alternate
		// path: the retry cannot succeed for a different reason and can block
		// the single reconcile worker for up to another full client timeout.
		return safeHarnessAPIError("delete AppProject mapping", candidate, httpResponse, err)
	}
	return nil
}

func (sdkAgentReadinessChecker) Readiness(
	ctx context.Context,
	session *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
) (harnessAgentReadiness, error) {
	_ = ctx
	candidates := scopedPathAgentIdentifierCandidates(agentCR.Spec.Scope, agentIdentifier)
	if len(candidates) == 0 {
		return harnessAgentReadiness{}, fmt.Errorf("get GitOps agent: empty agent identifier")
	}

	var lastErr error
	for _, candidate := range candidates {
		agent, httpResponse, err := session.Client.AgentApi.AgentServiceForServerGet(
			session.AuthCtx,
			candidate,
			agentCR.Spec.AccountId,
			&nextgen.AgentsApiAgentServiceForServerGetOpts{
				OrgIdentifier:     optionalStr(agentCR.Spec.OrgId),
				ProjectIdentifier: optionalProjectIdentifierForAgentScope(agentCR.Spec.Scope, agentCR.Spec.ProjectId),
				Scope:             optional.NewString(agentCR.Spec.Scope),
				WithCredentials:   optional.NewBool(false),
			},
		)
		if err == nil {
			return readinessFromHarnessAgent(agent), nil
		}
		if isHarnessNotFound(httpResponse, err) {
			continue
		}
		lastErr = safeHarnessAPIError("get GitOps agent", candidate, httpResponse, err)
	}
	if lastErr != nil {
		return harnessAgentReadiness{}, lastErr
	}
	return harnessAgentReadiness{}, nil
}

func readinessFromHarnessAgent(agent nextgen.V1Agent) harnessAgentReadiness {
	readiness := harnessAgentReadiness{Exists: true}
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

func (r *HarnessGitopsAgentReconciler) reconcileAppProjectMapping(
	ctx context.Context,
	session *HarnessSession,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	agentIdentifier string,
	target *projectMappingTarget,
) (ctrl.Result, error) {
	argoProjectName := target.AppProject
	exists, err := r.appProjectExists(ctx, agentCR.Namespace, argoProjectName)
	if err != nil {
		conditionErr := r.setMappingCondition(
			ctx,
			agentCR,
			mappingReasonVerificationFailed,
			fmt.Sprintf("Unable to read AppProject %s/%s", agentCR.Namespace, argoProjectName),
		)
		return ctrl.Result{}, stderrors.Join(err, conditionErr)
	}
	if !exists {
		if err := r.setMappingCondition(
			ctx,
			agentCR,
			mappingReasonAppProjectNotFound,
			fmt.Sprintf("AppProject %s/%s does not exist", agentCR.Namespace, argoProjectName),
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.appProjectPendingRetryInterval()}, nil
	}

	agentReadiness, err := r.gitOpsAgentReadinessChecker().Readiness(
		ctx,
		session,
		agentCR,
		agentIdentifier,
	)
	if err != nil {
		conditionErr := r.setMappingCondition(
			ctx,
			agentCR,
			mappingReasonVerificationFailed,
			"Unable to verify that the Harness GitOps agent exists",
		)
		return ctrl.Result{}, stderrors.Join(err, conditionErr)
	}
	if !agentReadiness.Exists {
		if err := r.setMappingCondition(
			ctx,
			agentCR,
			mappingReasonAgentNotFound,
			"Harness GitOps agent does not exist yet",
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.appProjectPendingRetryInterval()}, nil
	}
	if !agentReadiness.Ready {
		message := strings.TrimSpace(agentReadiness.Message)
		if message == "" {
			message = "Harness GitOps agent is not Connected and Healthy yet"
		}
		if err := r.setMappingCondition(
			ctx,
			agentCR,
			mappingReasonAgentNotHealthy,
			message,
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.appProjectPendingRetryInterval()}, nil
	}

	request := appProjectMappingRequestFor(agentCR, agentIdentifier, target)
	mapping, created, err := r.ensureAppProjectMapping(ctx, session, request)
	if err != nil {
		reason := mappingReasonVerificationFailed
		if stderrors.Is(err, errAppProjectMappingMismatch) {
			reason = mappingReasonMappingMismatch
		}
		conditionErr := r.setMappingCondition(
			ctx,
			agentCR,
			reason,
			err.Error(),
		)
		return ctrl.Result{}, stderrors.Join(err, conditionErr)
	}

	reason := mappingReasonMappingVerified
	if created {
		reason = mappingReasonMappingCreated
	}
	if err := r.setVerifiedMappingStatus(ctx, agentCR, mapping, reason, created); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.harnessMappingResyncInterval()}, nil
}

func (r *HarnessGitopsAgentReconciler) ensureAppProjectMapping(
	ctx context.Context,
	session *HarnessSession,
	request appProjectMappingRequest,
) (nextgen.V1AppProjectMappingV2, bool, error) {
	mappings, err := r.appProjectMappingAPI().List(ctx, session, request)
	if err != nil {
		return nextgen.V1AppProjectMappingV2{}, false, err
	}
	mapping, err := selectExactAppProjectMapping(mappings, request)
	if err == nil {
		return mapping, false, nil
	}
	if !stderrors.Is(err, errArgoProjectMappingNotFound) {
		return nextgen.V1AppProjectMappingV2{}, false, err
	}

	createErr := r.appProjectMappingAPI().Create(ctx, session, request)
	if createErr != nil && !stderrors.Is(createErr, errAppProjectMappingAlreadyExists) {
		return nextgen.V1AppProjectMappingV2{}, false, createErr
	}

	// Creation and AlreadyExists are both untrusted until a fresh list proves
	// the exact agent/account/org/project/AppProject tuple and returns its ID.
	mappings, err = r.appProjectMappingAPI().List(ctx, session, request)
	if err != nil {
		return nextgen.V1AppProjectMappingV2{}, false, err
	}
	mapping, err = selectExactAppProjectMapping(mappings, request)
	if err != nil {
		if stderrors.Is(err, errArgoProjectMappingNotFound) {
			return nextgen.V1AppProjectMappingV2{}, false, fmt.Errorf(
				"%w: create/list completed but the exact mapping is absent",
				errAppProjectMappingNotVerified,
			)
		}
		return nextgen.V1AppProjectMappingV2{}, false, err
	}
	return mapping, createErr == nil, nil
}

func selectExactAppProjectMapping(
	mappings []nextgen.V1AppProjectMappingV2,
	request appProjectMappingRequest,
) (nextgen.V1AppProjectMappingV2, error) {
	var exact []nextgen.V1AppProjectMappingV2
	mismatch := false
	for _, mapping := range mappings {
		if strings.TrimSpace(mapping.ArgoProjectName) != request.ArgoProjectName {
			continue
		}

		if agentIdentifiersEquivalent(request.AgentScope, mapping.AgentIdentifier, request.AgentIdentifier) &&
			strings.TrimSpace(mapping.AccountIdentifier) == request.AccountIdentifier &&
			strings.TrimSpace(mapping.OrgIdentifier) == request.Mapping.OrgIdentifier &&
			strings.TrimSpace(mapping.ProjectIdentifier) == request.Mapping.ProjectIdentifier &&
			strings.TrimSpace(mapping.Identifier) != "" {
			exact = append(exact, mapping)
			continue
		}
		mismatch = true
	}

	if len(exact) > 0 {
		sort.Slice(exact, func(i, j int) bool {
			return exact[i].Identifier < exact[j].Identifier
		})
		return exact[0], nil
	}
	if mismatch {
		return nextgen.V1AppProjectMappingV2{}, fmt.Errorf(
			"%w: AppProject %q is not mapped to account=%q org=%q project=%q agent=%q",
			errAppProjectMappingMismatch,
			request.ArgoProjectName,
			request.AccountIdentifier,
			request.Mapping.OrgIdentifier,
			request.Mapping.ProjectIdentifier,
			request.AgentIdentifier,
		)
	}
	return nextgen.V1AppProjectMappingV2{}, errArgoProjectMappingNotFound
}

func agentIdentifiersEquivalent(scope string, left string, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	for _, candidate := range scopedPathAgentIdentifierCandidates(scope, right) {
		if left == candidate {
			return true
		}
	}
	return false
}

func isAppProjectMappingAlreadyExists(response *http.Response, err error) bool {
	if response != nil && response.StatusCode == http.StatusConflict {
		return true
	}
	swaggerErr, ok := err.(nextgen.GenericSwaggerError)
	if !ok {
		return false
	}
	return strings.Contains(strings.ToLower(string(swaggerErr.Body())), "already exists")
}

func isHarnessNotFound(response *http.Response, err error) bool {
	if response != nil && response.StatusCode == http.StatusNotFound {
		return true
	}
	swaggerErr, ok := err.(nextgen.GenericSwaggerError)
	if !ok {
		return false
	}
	return strings.Contains(strings.ToLower(string(swaggerErr.Body())), "not found")
}

func safeHarnessAPIError(operation string, agentIdentifier string, response *http.Response, err error) error {
	if response != nil {
		return fmt.Errorf(
			"%s for agentIdentifier=%q failed with HTTP %d: %w",
			operation,
			agentIdentifier,
			response.StatusCode,
			err,
		)
	}
	return fmt.Errorf("%s for agentIdentifier=%q failed: %w", operation, agentIdentifier, err)
}

// setMappingCondition records a not-ready MappingReady condition. MappingReady
// is only ever set to True by setVerifiedMappingStatus, so this helper always
// writes ConditionFalse.
func (r *HarnessGitopsAgentReconciler) setMappingCondition(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	reason string,
	message string,
) error {
	changed := apiMeta.SetStatusCondition(&agentCR.Status.Conditions, metav1.Condition{
		Type:               mappingReadyConditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: agentCR.Generation,
		Reason:             reason,
		Message:            message,
	})
	if !changed {
		return nil
	}
	return r.Status().Update(ctx, agentCR)
}

func (r *HarnessGitopsAgentReconciler) setVerifiedMappingStatus(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
	mapping nextgen.V1AppProjectMappingV2,
	reason string,
	created bool,
) error {
	mappingOwnership := infrastructurev1.OwnershipExternal
	if created ||
		(agentCR.Status.ArgoProjectMappingId == mapping.Identifier &&
			agentCR.Status.ArgoProjectMappingOwnership == infrastructurev1.OwnershipManaged) {
		mappingOwnership = infrastructurev1.OwnershipManaged
	}
	fieldsChanged := agentCR.Status.ArgoProjectId != mapping.ArgoProjectName ||
		agentCR.Status.ArgoProjectMappingId != mapping.Identifier ||
		agentCR.Status.ArgoProjectMappingOwnership != mappingOwnership ||
		agentCR.Status.ArgoProjectMappingOrgId != strings.TrimSpace(mapping.OrgIdentifier) ||
		agentCR.Status.ArgoProjectMappingProjectId != strings.TrimSpace(mapping.ProjectIdentifier)
	agentCR.Status.ArgoProjectId = mapping.ArgoProjectName
	agentCR.Status.ArgoProjectMappingId = mapping.Identifier
	agentCR.Status.ArgoProjectMappingOwnership = mappingOwnership
	agentCR.Status.ArgoProjectMappingOrgId = strings.TrimSpace(mapping.OrgIdentifier)
	agentCR.Status.ArgoProjectMappingProjectId = strings.TrimSpace(mapping.ProjectIdentifier)
	conditionChanged := apiMeta.SetStatusCondition(&agentCR.Status.Conditions, metav1.Condition{
		Type:               mappingReadyConditionType,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: agentCR.Generation,
		Reason:             reason,
		Message:            "Harness AppProject mapping exists and matches the desired tuple",
	})
	if !fieldsChanged && !conditionChanged {
		return nil
	}
	return r.Status().Update(ctx, agentCR)
}

func (r *HarnessGitopsAgentReconciler) setMappingRemovedStatus(
	ctx context.Context,
	agentCR *infrastructurev1.HarnessGitopsAgent,
) error {
	fieldsChanged := agentCR.Status.ArgoProjectId != "" ||
		agentCR.Status.ArgoProjectMappingId != "" ||
		agentCR.Status.ArgoProjectMappingOwnership != "" ||
		agentCR.Status.ArgoProjectMappingOrgId != "" ||
		agentCR.Status.ArgoProjectMappingProjectId != ""
	agentCR.Status.ArgoProjectId = ""
	agentCR.Status.ArgoProjectMappingId = ""
	agentCR.Status.ArgoProjectMappingOwnership = ""
	agentCR.Status.ArgoProjectMappingOrgId = ""
	agentCR.Status.ArgoProjectMappingProjectId = ""
	conditionChanged := apiMeta.SetStatusCondition(&agentCR.Status.Conditions, metav1.Condition{
		Type:               mappingReadyConditionType,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: agentCR.Generation,
		Reason:             mappingReasonMappingRemoved,
		Message:            "No Harness AppProject mapping is desired",
	})
	if !fieldsChanged && !conditionChanged {
		return nil
	}
	return r.Status().Update(ctx, agentCR)
}
