package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

const (
	agentDependencyNamespace = "agent-dependency-tests"
	agentDependencyName      = "shared-agent"
	projectMappingFinalizer  = "infrastructure.kandylis.co.uk/project-mapping-finalizer"
)

func TestAgentMappingDependenciesUseFreshReaderAndWait(t *testing.T) {
	agent := newAgentDependencyTestAgent()
	references := []client.Object{
		newAgentDependencyTestMapping("z-last", agentDependencyNamespace, agentDependencyName, false),
		newAgentDependencyTestMapping("a-first", agentDependencyNamespace, agentDependencyName, true),
		newAgentDependencyTestMapping("other-agent", agentDependencyNamespace, "another-agent", false),
		newAgentDependencyTestMapping("other-namespace", "another-namespace", agentDependencyName, false),
	}
	fixture := newAgentDependencyFixture(t, agent, references, nil, nil, nil)

	result, done, err := fixture.reconciler.reconcileMappingDependenciesForDeletion(
		context.Background(),
		fixture.getAgent(t),
	)
	if err != nil {
		t.Fatalf("reconcile dependencies: %v", err)
	}
	if !done || result.RequeueAfter != agentMappingDependencyRetryInterval {
		t.Fatalf("result = (%+v, done=%t), want dependency wait", result, done)
	}
	if got := strings.Join(*fixture.deleted, ","); got != "z-last" {
		t.Fatalf("deleted mappings = %q, want z-last", got)
	}

	current := fixture.getAgent(t)
	condition := apiMeta.FindStatusCondition(current.Status.Conditions, harnessAgentReadyCondition)
	if condition == nil {
		t.Fatal("Agent Ready condition is missing")
	}
	if condition.Status != metav1.ConditionFalse ||
		condition.Reason != harnessAgentReasonWaitingForMappings {
		t.Fatalf("unexpected Agent Ready condition: %#v", condition)
	}
	if condition.Message !=
		"Waiting for HarnessGitopsProjectMapping resources to be deleted: a-first, z-last" {
		t.Fatalf("condition message is not stable: %q", condition.Message)
	}
	if !controllerutil.ContainsFinalizer(current, harnessAgentFinalizer) {
		t.Fatal("Agent finalizer was removed while Mapping dependencies remained")
	}

	before := *fixture.statusUpdates
	if _, _, err := fixture.reconciler.reconcileMappingDependenciesForDeletion(
		context.Background(),
		fixture.getAgent(t),
	); err != nil {
		t.Fatalf("repeat dependency reconcile: %v", err)
	}
	if got := *fixture.statusUpdates; got != before {
		t.Fatalf("stable condition caused a status update: got %d, want %d", got, before)
	}
}

func TestAgentMappingDependenciesReturnDeleteError(t *testing.T) {
	deleteErr := errors.New("delete failed")
	agent := newAgentDependencyTestAgent()
	fixture := newAgentDependencyFixture(
		t,
		agent,
		[]client.Object{
			newAgentDependencyTestMapping(
				"blocked-mapping",
				agentDependencyNamespace,
				agentDependencyName,
				false,
			),
		},
		map[string]error{"blocked-mapping": deleteErr},
		nil,
		nil,
	)

	result, done, err := fixture.reconciler.reconcileMappingDependenciesForDeletion(
		context.Background(),
		fixture.getAgent(t),
	)
	if !done || result.RequeueAfter == 0 {
		t.Fatalf("result = (%+v, done=%t), want retained dependency wait", result, done)
	}
	if !errors.Is(err, deleteErr) {
		t.Fatalf("error = %v, want delete error", err)
	}
	current := fixture.getAgent(t)
	if !controllerutil.ContainsFinalizer(current, harnessAgentFinalizer) {
		t.Fatal("Agent finalizer was removed after Mapping delete failed")
	}
	if apiMeta.FindStatusCondition(current.Status.Conditions, harnessAgentReadyCondition) == nil {
		t.Fatal("waiting condition was not recorded after Mapping delete failed")
	}
}

func TestAgentMappingDependenciesReturnStatusError(t *testing.T) {
	statusErr := errors.New("status failed")
	agent := newAgentDependencyTestAgent()
	fixture := newAgentDependencyFixture(
		t,
		agent,
		[]client.Object{
			newAgentDependencyTestMapping(
				"deleting-mapping",
				agentDependencyNamespace,
				agentDependencyName,
				true,
			),
		},
		nil,
		statusErr,
		nil,
	)

	result, done, err := fixture.reconciler.reconcileMappingDependenciesForDeletion(
		context.Background(),
		fixture.getAgent(t),
	)
	if !done || result.RequeueAfter == 0 {
		t.Fatalf("result = (%+v, done=%t), want retained dependency wait", result, done)
	}
	if !errors.Is(err, statusErr) {
		t.Fatalf("error = %v, want status error", err)
	}
	if !controllerutil.ContainsFinalizer(fixture.getAgent(t), harnessAgentFinalizer) {
		t.Fatal("Agent finalizer was removed after status update failed")
	}
}

func TestAgentMappingDependenciesReturnListError(t *testing.T) {
	listErr := errors.New("fresh list failed")
	agent := newAgentDependencyTestAgent()
	fixture := newAgentDependencyFixture(t, agent, nil, nil, nil, listErr)

	result, done, err := fixture.reconciler.reconcileMappingDependenciesForDeletion(
		context.Background(),
		fixture.getAgent(t),
	)
	if !done || !result.IsZero() {
		t.Fatalf("result = (%+v, done=%t), want retained error stop", result, done)
	}
	if !errors.Is(err, listErr) {
		t.Fatalf("error = %v, want list error", err)
	}
	if len(*fixture.deleted) != 0 || *fixture.statusUpdates != 0 {
		t.Fatal("list failure triggered Mapping deletion or Agent status mutation")
	}
	if !controllerutil.ContainsFinalizer(fixture.getAgent(t), harnessAgentFinalizer) {
		t.Fatal("Agent finalizer was removed after fresh list failed")
	}
}

func TestAgentMappingDependenciesProceedWhenClear(t *testing.T) {
	agent := newAgentDependencyTestAgent()
	fixture := newAgentDependencyFixture(
		t,
		agent,
		[]client.Object{
			newAgentDependencyTestMapping(
				"unrelated",
				agentDependencyNamespace,
				"another-agent",
				false,
			),
		},
		nil,
		nil,
		nil,
	)

	result, done, err := fixture.reconciler.reconcileMappingDependenciesForDeletion(
		context.Background(),
		fixture.getAgent(t),
	)
	if err != nil {
		t.Fatalf("reconcile dependencies: %v", err)
	}
	if done || !result.IsZero() {
		t.Fatalf("result = (%+v, done=%t), want Agent deletion to proceed", result, done)
	}
	if len(*fixture.deleted) != 0 || *fixture.statusUpdates != 0 {
		t.Fatal("clear dependency check mutated Mapping or Agent resources")
	}
}

func TestAgentDeletionWaitsForMappingsBeforeOwnershipDecision(t *testing.T) {
	tests := []struct {
		name          string
		ownership     infrastructurev1.ResourceOwnership
		existingAgent string
	}{
		{
			name:      "managed agent",
			ownership: infrastructurev1.OwnershipManaged,
		},
		{
			name:          "external agent",
			ownership:     infrastructurev1.OwnershipExternal,
			existingAgent: "existing-agent",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := newAgentDependencyTestAgent()
			agent.Status.AgentIdentifier = "agent-id"
			agent.Status.AgentOwnership = test.ownership
			fixture := newAgentDependencyFixture(
				t,
				agent,
				[]client.Object{
					newAgentDependencyTestMapping(
						"dependent-mapping",
						agentDependencyNamespace,
						agentDependencyName,
						false,
					),
				},
				nil,
				nil,
				nil,
			)

			result, err := fixture.reconciler.reconcileDeletion(
				context.Background(),
				fixture.getAgent(t),
				test.existingAgent,
				test.existingAgent != "",
			)
			if err != nil {
				t.Fatalf("reconcile deletion: %v", err)
			}
			if result.RequeueAfter != agentMappingDependencyRetryInterval {
				t.Fatalf("requeueAfter = %s, want %s", result.RequeueAfter, agentMappingDependencyRetryInterval)
			}
			if got := strings.Join(*fixture.deleted, ","); got != "dependent-mapping" {
				t.Fatalf("deleted mappings = %q, want dependent-mapping", got)
			}
			if !controllerutil.ContainsFinalizer(fixture.getAgent(t), harnessAgentFinalizer) {
				t.Fatal("Agent finalizer was removed before its Mapping dependency")
			}
		})
	}
}

type agentDependencyFixture struct {
	reconciler    *Reconciler
	key           client.ObjectKey
	deleted       *[]string
	statusUpdates *int
}

func newAgentDependencyFixture(
	t *testing.T,
	agent *infrastructurev1.HarnessGitopsAgent,
	readerObjects []client.Object,
	deleteErrors map[string]error,
	statusErr error,
	listErr error,
) *agentDependencyFixture {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := infrastructurev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add operator scheme: %v", err)
	}

	deleted := []string{}
	statusUpdates := 0
	cachedClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&infrastructurev1.HarnessGitopsAgent{}).
		WithObjects(agent).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(
				_ context.Context,
				_ client.WithWatch,
				obj client.Object,
				_ ...client.DeleteOption,
			) error {
				deleted = append(deleted, obj.GetName())
				return deleteErrors[obj.GetName()]
			},
			SubResourceUpdate: func(
				ctx context.Context,
				base client.Client,
				subResourceName string,
				obj client.Object,
				opts ...client.SubResourceUpdateOption,
			) error {
				if subResourceName == "status" {
					statusUpdates++
					if statusErr != nil {
						return statusErr
					}
				}
				return base.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}).
		Build()

	readerClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(readerObjects...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(
				ctx context.Context,
				base client.WithWatch,
				list client.ObjectList,
				opts ...client.ListOption,
			) error {
				if listErr != nil {
					return listErr
				}
				return base.List(ctx, list, opts...)
			},
		}).
		Build()

	return &agentDependencyFixture{
		reconciler: &Reconciler{
			Client:    cachedClient,
			Scheme:    scheme,
			APIReader: readerClient,
		},
		key:           client.ObjectKeyFromObject(agent),
		deleted:       &deleted,
		statusUpdates: &statusUpdates,
	}
}

func (f *agentDependencyFixture) getAgent(
	t *testing.T,
) *infrastructurev1.HarnessGitopsAgent {
	t.Helper()
	agent := &infrastructurev1.HarnessGitopsAgent{}
	if err := f.reconciler.Get(context.Background(), f.key, agent); err != nil {
		t.Fatalf("get Agent: %v", err)
	}
	return agent
}

func newAgentDependencyTestAgent() *infrastructurev1.HarnessGitopsAgent {
	return &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:       agentDependencyName,
			Namespace:  agentDependencyNamespace,
			Generation: 3,
			Finalizers: []string{harnessAgentFinalizer},
		},
	}
}

func newAgentDependencyTestMapping(
	name string,
	namespace string,
	agentName string,
	deleting bool,
) *infrastructurev1.HarnessGitopsProjectMapping {
	mapping := &infrastructurev1.HarnessGitopsProjectMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: infrastructurev1.HarnessGitopsProjectMappingSpec{
			AgentRef:   infrastructurev1.HarnessGitopsAgentReference{Name: agentName},
			AppProject: "default",
		},
	}
	if deleting {
		now := metav1.Now()
		mapping.DeletionTimestamp = &now
		mapping.Finalizers = []string{projectMappingFinalizer}
	}
	return mapping
}
