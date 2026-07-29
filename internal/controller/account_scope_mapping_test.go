package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/harness/harness-go-sdk/harness/nextgen"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

// B12: an ACCOUNT-scoped agent has no org of its own, so the org that owns the
// MAPPED project cannot be inherited from the agent. These tests pin the two
// scopes apart -- the agent scope (which locates the agent for List) and the
// mapping scope (which describes the project for Create and the comparison).

const accountScopeMappingOrg = "harness_controllers"

// newAccountScopeAgent derives an ACCOUNT-scoped CR from the ORG-scope fixture.
// spec.orgId and spec.projectId are deliberately absent: that is the contract
// for ACCOUNT scope, not an omission.
func newAccountScopeAgent(mappingOrgID string) *infrastructurev1.HarnessGitopsAgent {
	agent := newMappingTestAgent("account-mapping-resource", mappingTestNamespace, mappingTestAppProject)
	agent.Spec.Scope = "ACCOUNT"
	agent.Spec.OrgId = ""
	agent.Spec.ProjectId = ""
	agent.Spec.ProjectMapping.OrgId = mappingOrgID
	return agent
}

// accountScopeMappingRecords is what Harness returns for a correct mapping: the
// agent is addressed with the account. prefix, and the org/project describe the
// MAPPED project rather than the agent.
func accountScopeMappingRecords(identifier string, orgID string) []nextgen.V1AppProjectMappingV2 {
	return []nextgen.V1AppProjectMappingV2{
		mappingTestRecord(identifier, "account.", orgID, mappingTestProject),
	}
}

// newAccountScopeReconciler delegates to the shared builder with the fixtures
// every account-scope test wants: a ready agent and the user-created API key
// Secret.
func newAccountScopeReconciler(
	t *testing.T,
	agent *infrastructurev1.HarnessGitopsAgent,
	mappingAPI *fakeAppProjectMappingAPI,
	withAppProject bool,
) (*HarnessGitopsAgentReconciler, *infrastructurev1.HarnessGitopsAgent) {
	t.Helper()
	return newMappingTestReconciler(t, agent, withAppProject, mappingAPI, newReadyAgentChecker(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "api-key", Namespace: mappingTestNamespace},
			Data:       map[string][]byte{"api_key": []byte("not-a-real-key")},
		})
}

func getAgentByName(t *testing.T, k8sClient client.Client, name string) *infrastructurev1.HarnessGitopsAgent {
	t.Helper()
	fetched := &infrastructurev1.HarnessGitopsAgent{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{
		Namespace: mappingTestNamespace,
		Name:      name,
	}, fetched); err != nil {
		t.Fatalf("get agent %s: %v", name, err)
	}
	return fetched
}

// TestAccountScopeMappingUsesMappingOrg is the core B12 assertion: the org that
// reaches the comparison is the MAPPING's org, and the org that reaches the List
// query is the AGENT's (empty at ACCOUNT scope).
func TestAccountScopeMappingUsesMappingOrg(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{
		{mappings: accountScopeMappingRecords("mapping-1", accountScopeMappingOrg)},
	}}
	reconciler, agent := newAccountScopeReconciler(
		t, newAccountScopeAgent(accountScopeMappingOrg), mappingAPI, true)

	if _, err := reconcileMappingForTest(t, reconciler, agent); err != nil {
		t.Fatalf("expected the existing mapping to be adopted, got %v", err)
	}

	// Adopted, not recreated. MappingCreated here would mean the mismatch guard
	// let a duplicate through.
	if mappingAPI.createCalls != 0 {
		t.Fatalf("existing mapping was recreated: %d create calls", mappingAPI.createCalls)
	}
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionTrue, mappingReasonMappingVerified)

	if len(mappingAPI.listRequests) != 1 {
		t.Fatalf("expected exactly one List, got %d", len(mappingAPI.listRequests))
	}
	request := mappingAPI.listRequests[0]

	// The List call locates the AGENT. At ACCOUNT scope it has no org/project.
	if request.Agent.OrgIdentifier != "" || request.Agent.ProjectIdentifier != "" {
		t.Fatalf("agent scope must be empty at ACCOUNT scope, got org=%q project=%q",
			request.Agent.OrgIdentifier, request.Agent.ProjectIdentifier)
	}
	// The comparison uses the MAPPING scope. This is the value that was "" before B12.
	if request.Mapping.OrgIdentifier != accountScopeMappingOrg {
		t.Fatalf("mapping org = %q, want %q", request.Mapping.OrgIdentifier, accountScopeMappingOrg)
	}
	if request.Mapping.ProjectIdentifier != mappingTestProject {
		t.Fatalf("mapping project = %q, want %q", request.Mapping.ProjectIdentifier, mappingTestProject)
	}
}

// TestAccountScopeMappingCreateCarriesMappingOrg covers the fresh-install path,
// where List finds nothing and Create is actually reached.
func TestAccountScopeMappingCreateCarriesMappingOrg(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{
		{},
		{mappings: accountScopeMappingRecords("mapping-1", accountScopeMappingOrg)},
	}}
	reconciler, agent := newAccountScopeReconciler(
		t, newAccountScopeAgent(accountScopeMappingOrg), mappingAPI, true)

	if _, err := reconcileMappingForTest(t, reconciler, agent); err != nil {
		t.Fatalf("expected create then verify to succeed, got %v", err)
	}
	if mappingAPI.listCalls != 2 || mappingAPI.createCalls != 1 {
		t.Fatalf("expected List -> Create -> List, got list=%d create=%d",
			mappingAPI.listCalls, mappingAPI.createCalls)
	}

	created := mappingAPI.createRequests[0]
	// Sending the agent scope here is the original B12 failure: Harness receives
	// a project with no org to resolve it under and maps nothing, silently.
	if created.Mapping.OrgIdentifier != accountScopeMappingOrg {
		t.Fatalf("create body org = %q, want %q", created.Mapping.OrgIdentifier, accountScopeMappingOrg)
	}
	if created.Mapping.ProjectIdentifier != mappingTestProject {
		t.Fatalf("create body project = %q, want %q", created.Mapping.ProjectIdentifier, mappingTestProject)
	}
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionTrue, mappingReasonMappingCreated)
}

// TestAccountScopeWithoutMappingOrgIsRejected: no org is resolvable, so the CR
// must fail loudly instead of producing an unmappable request.
func TestAccountScopeWithoutMappingOrgIsRejected(t *testing.T) {
	agent := newAccountScopeAgent("")

	target, err := projectMappingDetails(agent)
	if err == nil {
		t.Fatalf("expected validation to reject an ACCOUNT-scope mapping with no org, got target %+v", target)
	}
	if !strings.Contains(err.Error(), "projectMapping.orgId") {
		t.Fatalf("error should name the missing field, got %q", err)
	}
}

// TestAccountScopeInvalidMappingReportsAndSendsNothing: the failure must reach
// the CR status, and no Harness call may be made.
func TestAccountScopeInvalidMappingReportsAndSendsNothing(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{}
	reconciler, agent := newAccountScopeReconciler(t, newAccountScopeAgent(""), mappingAPI, true)

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(agent),
	})
	if err != nil {
		t.Fatalf("an invalid spec must not be retried as an error, got %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("an invalid spec must not requeue, got %+v", result)
	}
	if mappingAPI.listCalls != 0 || mappingAPI.createCalls != 0 {
		t.Fatalf("Harness was contacted despite an invalid spec: list=%d create=%d",
			mappingAPI.listCalls, mappingAPI.createCalls)
	}
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionFalse, mappingReasonInvalidProjectMapping)

	// The status write above triggers one follow-up event in the real cluster
	// (no event predicates are configured). This second pass must converge: the
	// condition is byte-identical, so SetStatusCondition reports no change, no
	// write happens, and no further event fires. A drifting LastTransitionTime
	// here would mean a status-update hot loop in production.
	before := getAgentByName(t, reconciler.Client, agent.Name).Status.DeepCopy()
	result, err = reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(agent),
	})
	if err != nil || result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("second reconcile of an invalid spec must be a quiet no-op, got result=%+v err=%v", result, err)
	}
	after := getAgentByName(t, reconciler.Client, agent.Name).Status
	if !apiequality.Semantic.DeepEqual(*before, after) {
		t.Fatalf("second reconcile churned status:\nbefore: %+v\nafter:  %+v", *before, after)
	}
}

// TestAccountScopeMappingWithDifferentOrgIsMismatch: the terminal mismatch
// branch must still refuse to touch a mapping pointing at another org.
func TestAccountScopeMappingWithDifferentOrgIsMismatch(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{
		{mappings: accountScopeMappingRecords("mapping-1", "some_other_org")},
	}}
	reconciler, agent := newAccountScopeReconciler(
		t, newAccountScopeAgent(accountScopeMappingOrg), mappingAPI, true)

	if _, err := reconcileMappingForTest(t, reconciler, agent); err == nil {
		t.Fatal("expected a mismatch error for a mapping owned by another org")
	}
	if mappingAPI.createCalls != 0 {
		t.Fatalf("a conflicting mapping was overwritten: %d create calls", mappingAPI.createCalls)
	}
	assertMappingCondition(t, reconciler.Client, agent.Name, metav1.ConditionFalse, mappingReasonMappingMismatch)
}

// TestOrgScopeWithoutAnyOrgPointsAtSpecOrgId: when neither org field is set on
// an ORG-scoped CR the actionable fix is spec.orgId -- the ACCOUNT-scope advice
// (spec.projectMapping.orgId) would send the operator to the wrong field.
func TestOrgScopeWithoutAnyOrgPointsAtSpecOrgId(t *testing.T) {
	agent := newMappingTestAgent("mapping-resource", mappingTestNamespace, mappingTestAppProject)
	agent.Spec.OrgId = ""

	_, err := projectMappingDetails(agent)
	if err == nil {
		t.Fatal("expected validation to reject an ORG-scope mapping with no resolvable org")
	}
	if !strings.Contains(err.Error(), "spec.orgId is required") {
		t.Fatalf("error should point at spec.orgId, got %q", err)
	}
}

// TestOrgScopeStillFallsBackToAgentOrg is the regression guard: ORG- and
// PROJECT-scoped CRs that never set projectMapping.orgId must be unchanged.
func TestOrgScopeStillFallsBackToAgentOrg(t *testing.T) {
	agent := newMappingTestAgent("mapping-resource", mappingTestNamespace, mappingTestAppProject)
	if agent.Spec.ProjectMapping.OrgId != "" {
		t.Fatal("fixture should not set projectMapping.orgId")
	}

	target, err := projectMappingDetails(agent)
	if err != nil {
		t.Fatalf("ORG scope with no mapping org must stay valid, got %v", err)
	}
	if target.OrgID != mappingTestOrg {
		t.Fatalf("expected fallback to spec.orgId %q, got %q", mappingTestOrg, target.OrgID)
	}

	// And both scopes should agree at ORG scope -- that coincidence is why the
	// single-org bug survived until ACCOUNT scope exposed it.
	request := appProjectMappingRequestFor(agent, mappingTestAgentID, target)
	if request.Agent.OrgIdentifier != mappingTestOrg || request.Mapping.OrgIdentifier != mappingTestOrg {
		t.Fatalf("ORG scope should populate both orgs, got agent=%q mapping=%q",
			request.Agent.OrgIdentifier, request.Mapping.OrgIdentifier)
	}
}

// TestDeleteUsesLiveMappingIdNotStatus: the delete path must re-List. A
// remembered ID proves a row exists, not that it is still ours.
func TestDeleteUsesLiveMappingIdNotStatus(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{
		{mappings: accountScopeMappingRecords("live-id", accountScopeMappingOrg)},
	}}
	agent := newAccountScopeAgent(accountScopeMappingOrg)
	agent.Status.ArgoProjectMappingId = "stale-id"
	reconciler, fetched := newAccountScopeReconciler(t, agent, mappingAPI, true)

	target := mustProjectMappingTarget(t, fetched)
	if err := reconciler.deleteAppProjectMapping(
		context.Background(), nil, fetched, mappingTestAgentID, target,
	); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if mappingAPI.deleteCalls != 1 {
		t.Fatalf("expected exactly one delete, got %d", mappingAPI.deleteCalls)
	}
	if mappingAPI.deletedIDs[0] != "live-id" {
		t.Fatalf("deleted the remembered ID instead of the live one: %q", mappingAPI.deletedIDs[0])
	}
	if got := mappingAPI.deleteRequests[0].Mapping.OrgIdentifier; got != accountScopeMappingOrg {
		t.Fatalf("delete sent org %q, want %q", got, accountScopeMappingOrg)
	}
}

// TestDeleteRefusesMappingOwnedByAnotherOrg: refusing is correct, but it must
// not block the finalizer -- there is nothing of ours to clean up.
func TestDeleteRefusesMappingOwnedByAnotherOrg(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{listResults: []fakeMappingListResult{
		{mappings: accountScopeMappingRecords("mapping-1", "some_other_org")},
	}}
	agent := newAccountScopeAgent(accountScopeMappingOrg)
	reconciler, fetched := newAccountScopeReconciler(t, agent, mappingAPI, true)

	target := mustProjectMappingTarget(t, fetched)
	if err := reconciler.deleteAppProjectMapping(
		context.Background(), nil, fetched, mappingTestAgentID, target,
	); err != nil {
		t.Fatalf("a mismatched mapping must not fail deletion, got %v", err)
	}
	if mappingAPI.deleteCalls != 0 {
		t.Fatalf("deleted a mapping owned by another org: %d calls", mappingAPI.deleteCalls)
	}
}

// TestInvalidMappingDoesNotStrandFinalizer: validation runs after the deletion
// branch, so a CR that can never pass validation can still be deleted.
func TestInvalidMappingDoesNotStrandFinalizer(t *testing.T) {
	mappingAPI := &fakeAppProjectMappingAPI{}
	agent := newAccountScopeAgent("") // invalid: no resolvable org
	// existingAgentIdentifier keeps the shared agent alive, so deletion only has
	// to clean up the mapping -- no unfaked agent-delete call is made.
	agent.Spec.ExistingAgentIdentifier = mappingTestAgentID
	agent.Finalizers = []string{harnessAgentFinalizer}
	reconciler, fetched := newAccountScopeReconciler(t, agent, mappingAPI, true)

	if err := reconciler.Client.Delete(context.Background(), fetched); err != nil {
		t.Fatalf("delete CR: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(fetched),
	}); err != nil {
		t.Fatalf("deletion must not be blocked by an invalid spec, got %v", err)
	}

	remaining := &infrastructurev1.HarnessGitopsAgent{}
	err := reconciler.Client.Get(context.Background(),
		client.ObjectKeyFromObject(fetched), remaining)
	if err == nil && len(remaining.Finalizers) > 0 {
		t.Fatalf("finalizer was stranded on an invalid CR: %v", remaining.Finalizers)
	}
}
