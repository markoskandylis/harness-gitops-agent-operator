package controller

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/harness/harness-go-sdk/harness/nextgen"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

const (
	mappingTestNamespace  = "mapping-test"
	mappingTestAgentID    = "mapping-agent"
	mappingTestAccount    = "account"
	mappingTestOrg        = "org"
	mappingTestProject    = "platformteam"
	mappingTestAppProject = "default"
)

type fakeMappingListResult struct {
	mappings []nextgen.V1AppProjectMappingV2
	err      error
}

type fakeAppProjectMappingAPI struct {
	listResults []fakeMappingListResult
	createErr   error
	listCalls   int
	createCalls int
}

func (f *fakeAppProjectMappingAPI) List(
	_ context.Context,
	_ *HarnessSession,
	_ appProjectMappingRequest,
) ([]nextgen.V1AppProjectMappingV2, error) {
	index := f.listCalls
	f.listCalls++
	if index >= len(f.listResults) {
		return nil, nil
	}
	result := f.listResults[index]
	return result.mappings, result.err
}

func (f *fakeAppProjectMappingAPI) Create(
	_ context.Context,
	_ *HarnessSession,
	_ appProjectMappingRequest,
) error {
	f.createCalls++
	return f.createErr
}

type fakeAgentReadinessChecker struct {
	readiness harnessAgentReadiness
	err       error
	calls     int
}

func (f *fakeAgentReadinessChecker) Readiness(
	_ context.Context,
	_ *HarnessSession,
	_ *infrastructurev1.HarnessGitopsAgent,
	_ string,
) (harnessAgentReadiness, error) {
	f.calls++
	return f.readiness, f.err
}

func TestMappingWaitsForAppProject(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{}
	readinessChecker := newReadyAgentChecker()
	reconciler, agent := newMappingTestReconciler(t, false, mappingAPI, readinessChecker, infrastructurev1.HarnessGitopsAgentStatus{})

	result, err := reconcileMappingForTest(t, reconciler, agent)
	if err != nil {
		t.Fatalf("reconcile mapping: %v", err)
	}
	if result.RequeueAfter != DefaultAppProjectPendingRetryInterval {
		t.Fatalf("expected retry after %s, got %s", DefaultAppProjectPendingRetryInterval, result.RequeueAfter)
	}
	if mappingAPI.listCalls != 0 || mappingAPI.createCalls != 0 {
		t.Fatalf("mapping API was called before AppProject existed: list=%d create=%d", mappingAPI.listCalls, mappingAPI.createCalls)
	}
	if readinessChecker.calls != 0 {
		t.Fatalf("Harness agent was checked before AppProject existed: %d calls", readinessChecker.calls)
	}
	assertMappingCondition(t, reconciler.Client, metav1.ConditionFalse, mappingReasonAppProjectNotFound)
}

func TestMissingAppProjectRetainsVerifiedMappingIdentity(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{}
	readinessChecker := newReadyAgentChecker()
	reconciler, agent := newMappingTestReconciler(
		t,
		false,
		mappingAPI,
		readinessChecker,
		infrastructurev1.HarnessGitopsAgentStatus{
			ArgoProjectId:        mappingTestAppProject,
			ArgoProjectMappingId: "verified-mapping",
		},
	)

	if _, err := reconcileMappingForTest(t, reconciler, agent); err != nil {
		t.Fatalf("reconcile mapping: %v", err)
	}
	current := getMappingTestAgent(t, reconciler.Client)
	if current.Status.ArgoProjectId != mappingTestAppProject ||
		current.Status.ArgoProjectMappingId != "verified-mapping" {
		t.Fatalf(
			"temporary AppProject absence cleared verified mapping identity: %#v",
			current.Status,
		)
	}
	assertMappingCondition(t, reconciler.Client, metav1.ConditionFalse, mappingReasonAppProjectNotFound)
}

func TestMappingUsesConfiguredIntervals(t *testing.T) {
	retryInterval := 7 * time.Second
	retryReconciler, retryAgent := newMappingTestReconciler(
		t,
		false,
		&fakeAppProjectMappingAPI{},
		newReadyAgentChecker(),
		infrastructurev1.HarnessGitopsAgentStatus{},
	)
	retryReconciler.AppProjectPendingRetryInterval = retryInterval
	retryResult, err := reconcileMappingForTest(t, retryReconciler, retryAgent)
	if err != nil {
		t.Fatalf("reconcile missing AppProject: %v", err)
	}
	if retryResult.RequeueAfter != retryInterval {
		t.Fatalf("expected configured retry %s, got %s", retryInterval, retryResult.RequeueAfter)
	}

	resyncInterval := 7 * time.Minute
	resyncReconciler, resyncAgent := newMappingTestReconciler(
		t,
		true,
		&fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{{
			mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-existing", mappingTestProject)},
		}}},
		newReadyAgentChecker(),
		infrastructurev1.HarnessGitopsAgentStatus{},
	)
	resyncReconciler.HarnessMappingResyncInterval = resyncInterval
	resyncResult, err := reconcileMappingForTest(t, resyncReconciler, resyncAgent)
	if err != nil {
		t.Fatalf("reconcile existing mapping: %v", err)
	}
	if resyncResult.RequeueAfter != resyncInterval {
		t.Fatalf("expected configured resync %s, got %s", resyncInterval, resyncResult.RequeueAfter)
	}
}

func TestValidateMappingIntervals(t *testing.T) {
	if err := ValidateMappingIntervals(
		DefaultAppProjectPendingRetryInterval,
		DefaultHarnessMappingResyncInterval,
	); err != nil {
		t.Fatalf("validate defaults: %v", err)
	}
	if err := ValidateMappingIntervals(0, DefaultHarnessMappingResyncInterval); err == nil {
		t.Fatal("expected zero pending retry interval to be rejected")
	}
	if err := ValidateMappingIntervals(DefaultAppProjectPendingRetryInterval, 0); err == nil {
		t.Fatal("expected zero resync interval to be rejected")
	}
}

func TestMappingIsCreatedAndVerifiedAfterAppProjectAppears(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{
		{},
		{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-created", mappingTestProject)}},
	}}
	readinessChecker := newReadyAgentChecker()
	reconciler, agent := newMappingTestReconciler(t, true, mappingAPI, readinessChecker, infrastructurev1.HarnessGitopsAgentStatus{})

	result, err := reconcileMappingForTest(t, reconciler, agent)
	if err != nil {
		t.Fatalf("reconcile mapping: %v", err)
	}
	if result.RequeueAfter != DefaultHarnessMappingResyncInterval {
		t.Fatalf("expected periodic resync after %s, got %s", DefaultHarnessMappingResyncInterval, result.RequeueAfter)
	}
	if mappingAPI.listCalls != 2 || mappingAPI.createCalls != 1 {
		t.Fatalf("unexpected mapping API calls: list=%d create=%d", mappingAPI.listCalls, mappingAPI.createCalls)
	}
	assertVerifiedMappingStatus(t, reconciler.Client, "mapping-created", mappingReasonMappingCreated)
}

func TestExistingMatchingMappingIsRetrievedAndStored(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{
		{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-existing", mappingTestProject)}},
	}}
	reconciler, agent := newMappingTestReconciler(
		t,
		true,
		mappingAPI,
		newReadyAgentChecker(),
		infrastructurev1.HarnessGitopsAgentStatus{},
	)

	if _, err := reconcileMappingForTest(t, reconciler, agent); err != nil {
		t.Fatalf("reconcile mapping: %v", err)
	}
	if mappingAPI.listCalls != 1 || mappingAPI.createCalls != 0 {
		t.Fatalf("existing mapping was not read idempotently: list=%d create=%d", mappingAPI.listCalls, mappingAPI.createCalls)
	}
	assertVerifiedMappingStatus(t, reconciler.Client, "mapping-existing", mappingReasonMappingVerified)
}

func TestAlreadyExistsResponseRequiresFreshVerification(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{
		listResults: []fakeMappingListResult{
			{},
			{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-existing", mappingTestProject)}},
		},
		createErr: errAppProjectMappingAlreadyExists,
	}
	reconciler, agent := newMappingTestReconciler(
		t,
		true,
		mappingAPI,
		newReadyAgentChecker(),
		infrastructurev1.HarnessGitopsAgentStatus{},
	)

	if _, err := reconcileMappingForTest(t, reconciler, agent); err != nil {
		t.Fatalf("reconcile mapping: %v", err)
	}
	if mappingAPI.listCalls != 2 || mappingAPI.createCalls != 1 {
		t.Fatalf("AlreadyExists was not followed by verification: list=%d create=%d", mappingAPI.listCalls, mappingAPI.createCalls)
	}
	assertVerifiedMappingStatus(t, reconciler.Client, "mapping-existing", mappingReasonMappingVerified)
}

func TestExistingMappingToAnotherProjectFailsWithMismatch(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{
		{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-wrong", "another-project")}},
	}}
	reconciler, agent := newMappingTestReconciler(
		t,
		true,
		mappingAPI,
		newReadyAgentChecker(),
		infrastructurev1.HarnessGitopsAgentStatus{ArgoProjectMappingId: "stale-id"},
	)

	_, err := reconcileMappingForTest(t, reconciler, agent)
	if !stderrors.Is(err, errAppProjectMappingMismatch) {
		t.Fatalf("expected MappingMismatch, got %v", err)
	}
	if mappingAPI.createCalls != 0 {
		t.Fatalf("conflicting mapping was overwritten: %d create calls", mappingAPI.createCalls)
	}
	assertMappingCondition(t, reconciler.Client, metav1.ConditionFalse, mappingReasonMappingMismatch)
	current := getMappingTestAgent(t, reconciler.Client)
	if current.Status.ArgoProjectMappingId != "" {
		t.Fatalf("stale mapping ID was retained after mismatch: %q", current.Status.ArgoProjectMappingId)
	}
}

func TestExternallyDeletedMappingIsRecreated(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{
		{},
		{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-recreated", mappingTestProject)}},
	}}
	reconciler, agent := newMappingTestReconciler(
		t,
		true,
		mappingAPI,
		newReadyAgentChecker(),
		infrastructurev1.HarnessGitopsAgentStatus{
			ArgoProjectId:        mappingTestAppProject,
			ArgoProjectMappingId: "mapping-deleted",
		},
	)

	if _, err := reconcileMappingForTest(t, reconciler, agent); err != nil {
		t.Fatalf("reconcile mapping: %v", err)
	}
	if mappingAPI.createCalls != 1 {
		t.Fatalf("deleted mapping was not recreated: %d create calls", mappingAPI.createCalls)
	}
	assertVerifiedMappingStatus(t, reconciler.Client, "mapping-recreated", mappingReasonMappingCreated)
}

func TestExternallyRecreatedMappingUpdatesStaleID(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{
		{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-new-id", mappingTestProject)}},
	}}
	reconciler, agent := newMappingTestReconciler(
		t,
		true,
		mappingAPI,
		newReadyAgentChecker(),
		infrastructurev1.HarnessGitopsAgentStatus{
			ArgoProjectId:        mappingTestAppProject,
			ArgoProjectMappingId: "mapping-old-id",
		},
	)

	if _, err := reconcileMappingForTest(t, reconciler, agent); err != nil {
		t.Fatalf("reconcile mapping: %v", err)
	}
	if mappingAPI.createCalls != 0 {
		t.Fatalf("matching external recreation triggered a duplicate create: %d calls", mappingAPI.createCalls)
	}
	assertVerifiedMappingStatus(t, reconciler.Client, "mapping-new-id", mappingReasonMappingVerified)
}

func TestMappingWaitsForHarnessAgentExistence(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{}
	readinessChecker := &fakeAgentReadinessChecker{readiness: harnessAgentReadiness{Exists: false}}
	reconciler, agent := newMappingTestReconciler(t, true, mappingAPI, readinessChecker, infrastructurev1.HarnessGitopsAgentStatus{})

	result, err := reconcileMappingForTest(t, reconciler, agent)
	if err != nil {
		t.Fatalf("reconcile mapping: %v", err)
	}
	if result.RequeueAfter != DefaultAppProjectPendingRetryInterval {
		t.Fatalf("expected retry after %s, got %s", DefaultAppProjectPendingRetryInterval, result.RequeueAfter)
	}
	if readinessChecker.calls != 1 {
		t.Fatalf("expected one Harness agent readiness check, got %d", readinessChecker.calls)
	}
	if mappingAPI.listCalls != 0 || mappingAPI.createCalls != 0 {
		t.Fatalf("mapping API called before the Harness agent existed: list=%d create=%d", mappingAPI.listCalls, mappingAPI.createCalls)
	}
	assertMappingCondition(t, reconciler.Client, metav1.ConditionFalse, mappingReasonAgentNotFound)
}

func TestMappingWaitsForHarnessAgentHealth(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{}
	readinessChecker := &fakeAgentReadinessChecker{readiness: harnessAgentReadiness{
		Exists:  true,
		Ready:   false,
		Message: "Harness GitOps agent is not ready: connection=CONNECTED health=UNHEALTHY",
	}}
	reconciler, agent := newMappingTestReconciler(
		t,
		true,
		mappingAPI,
		readinessChecker,
		infrastructurev1.HarnessGitopsAgentStatus{},
	)

	result, err := reconcileMappingForTest(t, reconciler, agent)
	if err != nil {
		t.Fatalf("reconcile mapping: %v", err)
	}
	if result.RequeueAfter != DefaultAppProjectPendingRetryInterval {
		t.Fatalf("expected retry after %s, got %s", DefaultAppProjectPendingRetryInterval, result.RequeueAfter)
	}
	if readinessChecker.calls != 1 {
		t.Fatalf("expected one Harness agent readiness check, got %d", readinessChecker.calls)
	}
	if mappingAPI.listCalls != 0 || mappingAPI.createCalls != 0 {
		t.Fatalf("mapping API called before the Harness agent was healthy: list=%d create=%d", mappingAPI.listCalls, mappingAPI.createCalls)
	}
	assertMappingCondition(t, reconciler.Client, metav1.ConditionFalse, mappingReasonAgentNotHealthy)
}

func TestCreateWithoutVerifiedListResultDoesNotBecomeReady(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{{}, {}}}
	reconciler, agent := newMappingTestReconciler(
		t,
		true,
		mappingAPI,
		newReadyAgentChecker(),
		infrastructurev1.HarnessGitopsAgentStatus{},
	)

	_, err := reconcileMappingForTest(t, reconciler, agent)
	if !stderrors.Is(err, errAppProjectMappingNotVerified) {
		t.Fatalf("expected unverified create to fail, got %v", err)
	}
	assertMappingCondition(t, reconciler.Client, metav1.ConditionFalse, mappingReasonVerificationFailed)
}

func newMappingTestReconciler(
	t *testing.T,
	withAppProject bool,
	mappingAPI *fakeAppProjectMappingAPI,
	readinessChecker *fakeAgentReadinessChecker,
	status infrastructurev1.HarnessGitopsAgentStatus,
) (*HarnessGitopsAgentReconciler, *infrastructurev1.HarnessGitopsAgent) {
	t.Helper()
	scheme := newMappingTestScheme(t)
	agent := newMappingTestAgent("mapping-resource", mappingTestNamespace, mappingTestAppProject)
	agent.Status = status

	objects := []client.Object{agent}
	if withAppProject {
		objects = append(objects, newAppProjectObject(mappingTestNamespace, mappingTestAppProject))
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&infrastructurev1.HarnessGitopsAgent{}).
		WithObjects(objects...).
		Build()
	reconciler := &HarnessGitopsAgentReconciler{
		Client:                k8sClient,
		Scheme:                scheme,
		mappingAPI:            mappingAPI,
		agentReadinessChecker: readinessChecker,
	}
	return reconciler, getMappingTestAgent(t, k8sClient)
}

func newReadyAgentChecker() *fakeAgentReadinessChecker {
	return &fakeAgentReadinessChecker{readiness: harnessAgentReadiness{
		Exists:  true,
		Ready:   true,
		Message: "Harness GitOps agent is Connected and Healthy",
	}}
}

func newMappingTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := infrastructurev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add HarnessGitopsAgent scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(appProjectGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		appProjectGVK.GroupVersion().WithKind("AppProjectList"),
		&unstructured.UnstructuredList{},
	)
	return scheme
}

func newMappingTestAgent(name string, namespace string, appProject string) *infrastructurev1.HarnessGitopsAgent {
	return &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: 1,
		},
		Spec: infrastructurev1.HarnessGitopsAgentSpec{
			Name:            mappingTestAgentID,
			Identifier:      mappingTestAgentID,
			Operator:        "ARGO",
			AccountId:       mappingTestAccount,
			OrgId:           mappingTestOrg,
			Scope:           "ORG",
			Type:            "MANAGED_ARGO_PROVIDER",
			ApiKeySecretRef: "api-key",
			TokenSecretRef:  "agent-token",
			ProjectMapping: &infrastructurev1.ProjectMappingSpec{
				ProjectId:  mappingTestProject,
				AppProject: appProject,
			},
		},
	}
}

func mappingTestRecord(identifier string, project string) nextgen.V1AppProjectMappingV2 {
	return nextgen.V1AppProjectMappingV2{
		Identifier:        identifier,
		AgentIdentifier:   "org." + mappingTestAgentID,
		AccountIdentifier: mappingTestAccount,
		OrgIdentifier:     mappingTestOrg,
		ProjectIdentifier: project,
		ArgoProjectName:   mappingTestAppProject,
	}
}

func reconcileMappingForTest(
	t *testing.T,
	reconciler *HarnessGitopsAgentReconciler,
	agent *infrastructurev1.HarnessGitopsAgent,
) (ctrl.Result, error) {
	t.Helper()
	return reconciler.reconcileAppProjectMapping(
		context.Background(),
		nil,
		agent,
		mappingTestAgentID,
		mappingTestAppProject,
		mappingTestProject,
	)
}

func getMappingTestAgent(t *testing.T, k8sClient client.Client) *infrastructurev1.HarnessGitopsAgent {
	t.Helper()
	agent := &infrastructurev1.HarnessGitopsAgent{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{
		Namespace: mappingTestNamespace,
		Name:      "mapping-resource",
	}, agent); err != nil {
		t.Fatalf("get mapping test agent: %v", err)
	}
	return agent
}

func assertMappingCondition(
	t *testing.T,
	k8sClient client.Client,
	status metav1.ConditionStatus,
	reason string,
) {
	t.Helper()
	agent := getMappingTestAgent(t, k8sClient)
	condition := apiMeta.FindStatusCondition(agent.Status.Conditions, mappingReadyConditionType)
	if condition == nil {
		t.Fatal("MappingReady condition is absent")
	}
	if condition.Status != status || condition.Reason != reason {
		t.Fatalf("unexpected MappingReady condition: %#v", condition)
	}
}

func assertVerifiedMappingStatus(
	t *testing.T,
	k8sClient client.Client,
	mappingID string,
	reason string,
) {
	t.Helper()
	agent := getMappingTestAgent(t, k8sClient)
	if agent.Status.ArgoProjectId != mappingTestAppProject ||
		agent.Status.ArgoProjectMappingId != mappingID {
		t.Fatalf("unexpected verified mapping status: %#v", agent.Status)
	}
	condition := apiMeta.FindStatusCondition(agent.Status.Conditions, mappingReadyConditionType)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != reason {
		t.Fatalf("unexpected MappingReady condition: %#v", condition)
	}
}
