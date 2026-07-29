package controller

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/harness/harness-go-sdk/harness/nextgen"
	corev1 "k8s.io/api/core/v1"
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
	deleteCalls int
	deletedIDs  []string
	// Captured requests. The agent/mapping scope split is only observable in
	// what actually reaches the API, so the ACCOUNT-scope tests assert on these.
	listRequests   []appProjectMappingRequest
	createRequests []appProjectMappingRequest
	deleteRequests []appProjectMappingRequest
}

func (f *fakeAppProjectMappingAPI) List(
	_ context.Context,
	_ *HarnessSession,
	request appProjectMappingRequest,
) ([]nextgen.V1AppProjectMappingV2, error) {
	index := f.listCalls
	f.listCalls++
	f.listRequests = append(f.listRequests, request)
	if index >= len(f.listResults) {
		return nil, nil
	}
	result := f.listResults[index]
	return result.mappings, result.err
}

func (f *fakeAppProjectMappingAPI) Create(
	_ context.Context,
	_ *HarnessSession,
	request appProjectMappingRequest,
) error {
	f.createCalls++
	f.createRequests = append(f.createRequests, request)
	return f.createErr
}

func (f *fakeAppProjectMappingAPI) Delete(
	_ context.Context,
	_ *HarnessSession,
	request appProjectMappingRequest,
	mappingID string,
) error {
	f.deleteCalls++
	f.deletedIDs = append(f.deletedIDs, mappingID)
	f.deleteRequests = append(f.deleteRequests, request)
	return nil
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
	reconciler, agent := newMappingTestReconciler(
		t,
		newMappingTestAgent("mapping-resource"),
		false,
		mappingAPI,
		readinessChecker,
	)

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
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionFalse, mappingReasonAppProjectNotFound)
}

func TestMissingAppProjectRetainsVerifiedMappingIdentity(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{}
	readinessChecker := newReadyAgentChecker()
	agent := newMappingTestAgent("mapping-resource")
	agent.Status = infrastructurev1.HarnessGitopsAgentStatus{
		ArgoProjectId:        mappingTestAppProject,
		ArgoProjectMappingId: "verified-mapping",
	}
	reconciler, agent := newMappingTestReconciler(t, agent, false, mappingAPI, readinessChecker)

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
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionFalse, mappingReasonAppProjectNotFound)
}

func TestMappingUsesConfiguredIntervals(t *testing.T) {
	retryInterval := 7 * time.Second
	retryReconciler, retryAgent := newMappingTestReconciler(
		t,
		newMappingTestAgent("mapping-resource"),
		false,
		&fakeAppProjectMappingAPI{},
		newReadyAgentChecker(),
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
		newMappingTestAgent("mapping-resource"),
		true,
		&fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{{
			mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-existing", "org.", mappingTestOrg, mappingTestProject)},
		}}},
		newReadyAgentChecker(),
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
		{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-created", "org.", mappingTestOrg, mappingTestProject)}},
	}}
	readinessChecker := newReadyAgentChecker()
	reconciler, agent := newMappingTestReconciler(
		t,
		newMappingTestAgent("mapping-resource"),
		true,
		mappingAPI,
		readinessChecker,
	)

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
		{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-existing", "org.", mappingTestOrg, mappingTestProject)}},
	}}
	reconciler, agent := newMappingTestReconciler(
		t,
		newMappingTestAgent("mapping-resource"),
		true,
		mappingAPI,
		newReadyAgentChecker(),
	)

	if _, err := reconcileMappingForTest(t, reconciler, agent); err != nil {
		t.Fatalf("reconcile mapping: %v", err)
	}
	if mappingAPI.listCalls != 1 || mappingAPI.createCalls != 0 {
		t.Fatalf("existing mapping was not read idempotently: list=%d create=%d", mappingAPI.listCalls, mappingAPI.createCalls)
	}
	assertVerifiedMappingStatus(t, reconciler.Client, "mapping-existing", mappingReasonMappingVerified)
}

func TestManagedMappingOwnershipSurvivesLaterVerification(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{
		{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-managed", "org.", mappingTestOrg, mappingTestProject)}},
	}}
	agent := newMappingTestAgent("mapping-resource")
	agent.Status = infrastructurev1.HarnessGitopsAgentStatus{
		ArgoProjectId:               mappingTestAppProject,
		ArgoProjectMappingId:        "mapping-managed",
		ArgoProjectMappingOwnership: infrastructurev1.OwnershipManaged,
	}
	reconciler, agent := newMappingTestReconciler(
		t,
		agent,
		true,
		mappingAPI,
		newReadyAgentChecker(),
	)

	if _, err := reconcileMappingForTest(t, reconciler, agent); err != nil {
		t.Fatalf("reconcile mapping: %v", err)
	}
	current := getMappingTestAgent(t, reconciler.Client)
	if current.Status.ArgoProjectMappingOwnership != infrastructurev1.OwnershipManaged {
		t.Fatalf("managed ownership was lost during verification: %#v", current.Status)
	}
}

func TestAlreadyExistsResponseRequiresFreshVerification(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{
		listResults: []fakeMappingListResult{
			{},
			{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-existing", "org.", mappingTestOrg, mappingTestProject)}},
		},
		createErr: errAppProjectMappingAlreadyExists,
	}
	reconciler, agent := newMappingTestReconciler(
		t,
		newMappingTestAgent("mapping-resource"),
		true,
		mappingAPI,
		newReadyAgentChecker(),
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
		{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-wrong", "org.", mappingTestOrg, "another-project")}},
	}}
	agent := newMappingTestAgent("mapping-resource")
	agent.Status = infrastructurev1.HarnessGitopsAgentStatus{ArgoProjectMappingId: "stale-id"}
	reconciler, agent := newMappingTestReconciler(t, agent, true, mappingAPI, newReadyAgentChecker())

	_, err := reconcileMappingForTest(t, reconciler, agent)
	if !stderrors.Is(err, errAppProjectMappingMismatch) {
		t.Fatalf("expected MappingMismatch, got %v", err)
	}
	if mappingAPI.createCalls != 0 {
		t.Fatalf("conflicting mapping was overwritten: %d create calls", mappingAPI.createCalls)
	}
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionFalse, mappingReasonMappingMismatch)
	current := getMappingTestAgent(t, reconciler.Client)
	if current.Status.ArgoProjectMappingId != "stale-id" {
		t.Fatalf("observed mapping identity was lost after mismatch: %q", current.Status.ArgoProjectMappingId)
	}
}

func TestExternallyDeletedMappingIsRecreated(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{
		{},
		{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-recreated", "org.", mappingTestOrg, mappingTestProject)}},
	}}
	agent := newMappingTestAgent("mapping-resource")
	agent.Status = infrastructurev1.HarnessGitopsAgentStatus{
		ArgoProjectId:        mappingTestAppProject,
		ArgoProjectMappingId: "mapping-deleted",
	}
	reconciler, agent := newMappingTestReconciler(t, agent, true, mappingAPI, newReadyAgentChecker())

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
		{mappings: []nextgen.V1AppProjectMappingV2{mappingTestRecord("mapping-new-id", "org.", mappingTestOrg, mappingTestProject)}},
	}}
	agent := newMappingTestAgent("mapping-resource")
	agent.Status = infrastructurev1.HarnessGitopsAgentStatus{
		ArgoProjectId:        mappingTestAppProject,
		ArgoProjectMappingId: "mapping-old-id",
	}
	reconciler, agent := newMappingTestReconciler(t, agent, true, mappingAPI, newReadyAgentChecker())

	if _, err := reconcileMappingForTest(t, reconciler, agent); err != nil {
		t.Fatalf("reconcile mapping: %v", err)
	}
	if mappingAPI.createCalls != 0 {
		t.Fatalf("matching external recreation triggered a duplicate create: %d calls", mappingAPI.createCalls)
	}
	assertVerifiedMappingStatus(t, reconciler.Client, "mapping-new-id", mappingReasonMappingVerified)
}

func TestRemovingManagedMappingDeletesObservedTupleBeforeConverging(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{{
		mappings: []nextgen.V1AppProjectMappingV2{
			mappingTestRecord("mapping-managed", "org.", mappingTestOrg, mappingTestProject),
		},
	}}}
	agent := newMappingTestAgent("mapping-resource")
	agent.Finalizers = []string{harnessAgentFinalizer}
	agent.Spec.ExistingAgentIdentifier = mappingTestAgentID
	agent.Spec.ProjectMapping = nil
	agent.Status = managedMappingTestStatus("mapping-managed")
	reconciler, agent := newMappingTestReconciler(
		t,
		agent,
		false,
		mappingAPI,
		newReadyAgentChecker(),
		mappingTestAPIKeySecret(),
	)

	result, err := reconciler.Reconcile(context.Background(), ctrlRequestFor(agent))
	if err != nil {
		t.Fatalf("remove managed mapping: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("mapping cleanup must requeue before further mutations: %+v", result)
	}
	if mappingAPI.listCalls != 1 || mappingAPI.deleteCalls != 1 || mappingAPI.createCalls != 0 {
		t.Fatalf("expected List -> Delete only, got list=%d delete=%d create=%d",
			mappingAPI.listCalls, mappingAPI.deleteCalls, mappingAPI.createCalls)
	}
	current := getMappingTestAgent(t, reconciler.Client)
	if current.Status.ArgoProjectMappingId != "" ||
		current.Status.ArgoProjectMappingOwnership != "" ||
		current.Status.ArgoProjectId != "" {
		t.Fatalf("removed mapping remained in status: %#v", current.Status)
	}
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionFalse, mappingReasonMappingRemoved)
}

func TestRemovingExternalMappingClearsStatusWithoutHarnessAPI(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{}
	agent := newMappingTestAgent("mapping-resource")
	agent.Finalizers = []string{harnessAgentFinalizer}
	agent.Spec.ExistingAgentIdentifier = mappingTestAgentID
	agent.Spec.ProjectMapping = nil
	agent.Status = infrastructurev1.HarnessGitopsAgentStatus{
		AgentIdentifier:             mappingTestAgentID,
		AgentOwnership:              infrastructurev1.OwnershipExternal,
		ArgoProjectId:               mappingTestAppProject,
		ArgoProjectMappingId:        "mapping-external",
		ArgoProjectMappingOwnership: infrastructurev1.OwnershipExternal,
		ArgoProjectMappingOrgId:     mappingTestOrg,
		ArgoProjectMappingProjectId: mappingTestProject,
	}
	reconciler, agent := newMappingTestReconciler(
		t,
		agent,
		false,
		mappingAPI,
		newReadyAgentChecker(),
	)

	if _, err := reconciler.Reconcile(context.Background(), ctrlRequestFor(agent)); err != nil {
		t.Fatalf("remove external mapping: %v", err)
	}
	if mappingAPI.listCalls != 0 || mappingAPI.deleteCalls != 0 {
		t.Fatalf("external mapping removal contacted Harness: list=%d delete=%d",
			mappingAPI.listCalls, mappingAPI.deleteCalls)
	}
	current := getMappingTestAgent(t, reconciler.Client)
	if current.Status.ArgoProjectId != "" ||
		current.Status.ArgoProjectMappingId != "" ||
		current.Status.ArgoProjectMappingOwnership != "" ||
		current.Status.ArgoProjectMappingOrgId != "" ||
		current.Status.ArgoProjectMappingProjectId != "" {
		t.Fatalf("removed external mapping remained in status: %#v", current.Status)
	}
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionFalse, mappingReasonMappingRemoved)
}

func TestRemovingPendingMappingClearsItsCondition(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{}
	agent := newMappingTestAgent("mapping-resource")
	agent.Finalizers = []string{harnessAgentFinalizer}
	agent.Spec.ExistingAgentIdentifier = mappingTestAgentID
	agent.Spec.ProjectMapping = nil
	agent.Status = infrastructurev1.HarnessGitopsAgentStatus{
		AgentIdentifier: mappingTestAgentID,
		AgentOwnership:  infrastructurev1.OwnershipExternal,
		Conditions: []metav1.Condition{{
			Type:               mappingReadyConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: agent.Generation,
			Reason:             mappingReasonAppProjectNotFound,
			Message:            "AppProject is pending",
		}},
	}
	reconciler, agent := newMappingTestReconciler(
		t,
		agent,
		false,
		mappingAPI,
		newReadyAgentChecker(),
	)

	if _, err := reconciler.Reconcile(context.Background(), ctrlRequestFor(agent)); err != nil {
		t.Fatalf("remove pending mapping: %v", err)
	}
	if mappingAPI.listCalls != 0 || mappingAPI.deleteCalls != 0 {
		t.Fatalf("pending mapping removal contacted Harness: list=%d delete=%d",
			mappingAPI.listCalls, mappingAPI.deleteCalls)
	}
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionFalse, mappingReasonMappingRemoved)
}

func TestChangingManagedMappingDeletesOldTupleBeforeCreatingNewOne(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{{
		mappings: []nextgen.V1AppProjectMappingV2{
			mappingTestRecord("mapping-managed", "org.", mappingTestOrg, mappingTestProject),
		},
	}}}
	agent := newMappingTestAgent("mapping-resource")
	agent.Finalizers = []string{harnessAgentFinalizer}
	agent.Spec.ExistingAgentIdentifier = mappingTestAgentID
	agent.Spec.ProjectMapping = &infrastructurev1.ProjectMappingSpec{
		ProjectId:  "new-project",
		AppProject: "new-app-project",
	}
	agent.Status = managedMappingTestStatus("mapping-managed")
	reconciler, agent := newMappingTestReconciler(
		t,
		agent,
		false,
		mappingAPI,
		newReadyAgentChecker(),
		mappingTestAPIKeySecret(),
	)

	result, err := reconciler.Reconcile(context.Background(), ctrlRequestFor(agent))
	if err != nil {
		t.Fatalf("change managed mapping: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("mapping replacement must requeue after deleting the old tuple: %+v", result)
	}
	if mappingAPI.deleteCalls != 1 || mappingAPI.createCalls != 0 {
		t.Fatalf("old and new mapping mutations were combined: delete=%d create=%d",
			mappingAPI.deleteCalls, mappingAPI.createCalls)
	}
	deleted := mappingAPI.deleteRequests[0]
	if deleted.ArgoProjectName != mappingTestAppProject ||
		deleted.Mapping.OrgIdentifier != mappingTestOrg ||
		deleted.Mapping.ProjectIdentifier != mappingTestProject {
		t.Fatalf("deleted the desired tuple instead of the observed old tuple: %#v", deleted)
	}
}

func TestRemovingManagedMappingFailsClosedWithoutResolvedTuple(t *testing.T) {
	agent := newMappingTestAgent("mapping-resource")
	agent.Finalizers = []string{harnessAgentFinalizer}
	agent.Spec.ExistingAgentIdentifier = mappingTestAgentID
	agent.Spec.ProjectMapping = nil
	agent.Status = infrastructurev1.HarnessGitopsAgentStatus{
		AgentIdentifier:             mappingTestAgentID,
		AgentOwnership:              infrastructurev1.OwnershipExternal,
		ArgoProjectId:               mappingTestAppProject,
		ArgoProjectMappingId:        "managed-without-scope",
		ArgoProjectMappingOwnership: infrastructurev1.OwnershipManaged,
	}
	mappingAPI := &fakeAppProjectMappingAPI{}
	reconciler, agent := newMappingTestReconciler(
		t,
		agent,
		false,
		mappingAPI,
		newReadyAgentChecker(),
	)

	_, err := reconciler.Reconcile(context.Background(), ctrlRequestFor(agent))
	if err == nil {
		t.Fatal("mapping removal should fail when the old scope cannot be proven")
	}
	if mappingAPI.listCalls != 0 || mappingAPI.deleteCalls != 0 {
		t.Fatalf("incomplete status triggered remote cleanup: list=%d delete=%d",
			mappingAPI.listCalls, mappingAPI.deleteCalls)
	}
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionFalse, mappingReasonCleanupBlocked)
}

func TestMappingWaitsForHarnessAgentExistence(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{}
	readinessChecker := &fakeAgentReadinessChecker{readiness: harnessAgentReadiness{Exists: false}}
	reconciler, agent := newMappingTestReconciler(
		t,
		newMappingTestAgent("mapping-resource"),
		true,
		mappingAPI,
		readinessChecker,
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
		t.Fatalf("mapping API called before the Harness agent existed: list=%d create=%d", mappingAPI.listCalls, mappingAPI.createCalls)
	}
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionFalse, mappingReasonAgentNotFound)
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
		newMappingTestAgent("mapping-resource"),
		true,
		mappingAPI,
		readinessChecker,
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
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionFalse, mappingReasonAgentNotHealthy)
}

func TestCreateWithoutVerifiedListResultDoesNotBecomeReady(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{{}, {}}}
	reconciler, agent := newMappingTestReconciler(
		t,
		newMappingTestAgent("mapping-resource"),
		true,
		mappingAPI,
		newReadyAgentChecker(),
	)

	_, err := reconcileMappingForTest(t, reconciler, agent)
	if !stderrors.Is(err, errAppProjectMappingNotVerified) {
		t.Fatalf("expected unverified create to fail, got %v", err)
	}
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionFalse, mappingReasonVerificationFailed)
}

func newMappingTestReconciler(
	t *testing.T,
	agent *infrastructurev1.HarnessGitopsAgent,
	withAppProject bool,
	mappingAPI *fakeAppProjectMappingAPI,
	readinessChecker *fakeAgentReadinessChecker,
	extraObjects ...client.Object,
) (*HarnessGitopsAgentReconciler, *infrastructurev1.HarnessGitopsAgent) {
	t.Helper()
	scheme := newMappingTestScheme(t)

	objects := append([]client.Object{agent}, extraObjects...)
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
	return reconciler, getAgentByName(t, k8sClient, agent.Name)
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
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(appProjectGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		appProjectGVK.GroupVersion().WithKind("AppProjectList"),
		&unstructured.UnstructuredList{},
	)
	return scheme
}

func newMappingTestAgent(name string) *infrastructurev1.HarnessGitopsAgent {
	return &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  mappingTestNamespace,
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
				AppProject: mappingTestAppProject,
			},
		},
	}
}

func mappingTestRecord(identifier string, agentPrefix string, orgID string, project string) nextgen.V1AppProjectMappingV2 {
	return nextgen.V1AppProjectMappingV2{
		Identifier:        identifier,
		AgentIdentifier:   agentPrefix + mappingTestAgentID,
		AccountIdentifier: mappingTestAccount,
		OrgIdentifier:     orgID,
		ProjectIdentifier: project,
		ArgoProjectName:   mappingTestAppProject,
	}
}

func managedMappingTestStatus(mappingID string) infrastructurev1.HarnessGitopsAgentStatus {
	return infrastructurev1.HarnessGitopsAgentStatus{
		AgentIdentifier:             mappingTestAgentID,
		AgentOwnership:              infrastructurev1.OwnershipExternal,
		ArgoProjectId:               mappingTestAppProject,
		ArgoProjectMappingId:        mappingID,
		ArgoProjectMappingOwnership: infrastructurev1.OwnershipManaged,
		ArgoProjectMappingOrgId:     mappingTestOrg,
		ArgoProjectMappingProjectId: mappingTestProject,
	}
}

func mappingTestAPIKeySecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "api-key", Namespace: mappingTestNamespace},
		Data:       map[string][]byte{"api_key": []byte("not-a-real-key")},
	}
}

func reconcileMappingForTest(
	t *testing.T,
	reconciler *HarnessGitopsAgentReconciler,
	agent *infrastructurev1.HarnessGitopsAgent,
) (ctrl.Result, error) {
	t.Helper()
	// Derive the target the same way Reconcile does, so these tests exercise
	// the real validation rather than a hand-built target that cannot fail.
	target := mustProjectMappingTarget(t, agent)
	return reconciler.reconcileAppProjectMapping(
		context.Background(),
		nil,
		agent,
		mappingTestAgentID,
		target,
	)
}

func mustProjectMappingTarget(t *testing.T, agent *infrastructurev1.HarnessGitopsAgent) *projectMappingTarget {
	t.Helper()
	target, err := projectMappingDetails(agent)
	if err != nil {
		t.Fatalf("projectMappingDetails: %v", err)
	}
	return target
}

func getMappingTestAgent(t *testing.T, k8sClient client.Client) *infrastructurev1.HarnessGitopsAgent {
	t.Helper()
	return getAgentByName(t, k8sClient, "mapping-resource")
}

// assertMappingCondition asserts the MappingReady condition on the named agent.
// Verified mapping IDs are asserted by assertVerifiedMappingStatus instead.
func assertMappingCondition(
	t *testing.T,
	k8sClient client.Client,
	name string,
	wantStatus metav1.ConditionStatus,
	wantReason string,
) {
	t.Helper()
	agent := getAgentByName(t, k8sClient, name)
	condition := apiMeta.FindStatusCondition(agent.Status.Conditions, mappingReadyConditionType)
	if condition == nil {
		t.Fatal("MappingReady condition is absent")
	}
	if condition.Status != wantStatus || condition.Reason != wantReason {
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
	wantOwnership := infrastructurev1.OwnershipExternal
	if reason == mappingReasonMappingCreated {
		wantOwnership = infrastructurev1.OwnershipManaged
	}
	if agent.Status.ArgoProjectMappingOwnership != wantOwnership {
		t.Fatalf("unexpected mapping ownership: got %q, want %q",
			agent.Status.ArgoProjectMappingOwnership, wantOwnership)
	}
	if agent.Status.ArgoProjectMappingOrgId != mappingTestOrg ||
		agent.Status.ArgoProjectMappingProjectId != mappingTestProject {
		t.Fatalf("resolved mapping scope was not stored: %#v", agent.Status)
	}
	condition := apiMeta.FindStatusCondition(agent.Status.Conditions, mappingReadyConditionType)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != reason {
		t.Fatalf("unexpected MappingReady condition: %#v", condition)
	}
}
