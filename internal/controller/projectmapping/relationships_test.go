package projectmapping

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

type recordingMappingFieldIndexer struct {
	object    client.Object
	field     string
	extractor client.IndexerFunc
	err       error
}

func (i *recordingMappingFieldIndexer) IndexField(
	_ context.Context,
	object client.Object,
	field string,
	extractor client.IndexerFunc,
) error {
	i.object = object
	i.field = field
	i.extractor = extractor
	return i.err
}

func TestRegisterProjectMappingAgentRefIndex(t *testing.T) {
	indexer := &recordingMappingFieldIndexer{}
	if err := registerProjectMappingAgentRefIndex(context.Background(), indexer); err != nil {
		t.Fatalf("register index: %v", err)
	}
	if _, ok := indexer.object.(*infrastructurev1.HarnessGitopsProjectMapping); !ok {
		t.Fatalf("indexed object = %T, want HarnessGitopsProjectMapping", indexer.object)
	}
	if indexer.field != projectMappingAgentRefIndexField {
		t.Fatalf("index field = %q, want %q", indexer.field, projectMappingAgentRefIndexField)
	}
	if projectMappingAgentRefIndexField != ".spec.agentRef.name" {
		t.Fatalf("index field contract changed to %q", projectMappingAgentRefIndexField)
	}
	if indexer.extractor == nil {
		t.Fatal("index extractor was not registered")
	}

	mapping := relationshipTestMapping("mapping", "namespace", "  shared-agent  ")
	if got := indexer.extractor(mapping); !reflect.DeepEqual(got, []string{"shared-agent"}) {
		t.Fatalf("index values = %#v, want shared-agent", got)
	}
	mapping.Spec.AgentRef.Name = ""
	if got := indexer.extractor(mapping); got != nil {
		t.Fatalf("empty reference index values = %#v, want nil", got)
	}
	if got := indexer.extractor(&infrastructurev1.HarnessGitopsAgent{}); got != nil {
		t.Fatalf("wrong object index values = %#v, want nil", got)
	}

	sentinel := errors.New("index registration failed")
	if err := registerProjectMappingAgentRefIndex(
		context.Background(),
		&recordingMappingFieldIndexer{err: sentinel},
	); !errors.Is(err, sentinel) {
		t.Fatalf("registration error = %v, want %v", err, sentinel)
	}
}

func TestAgentEventMapsToIndexedSameNamespaceMappingsInStableOrder(t *testing.T) {
	scheme := relationshipTestScheme(t)
	objects := []client.Object{
		relationshipTestMapping("z-last", "tenant-a", "shared-agent"),
		relationshipTestMapping("a-first", "tenant-a", "shared-agent"),
		relationshipTestMapping("other-agent", "tenant-a", "another-agent"),
		relationshipTestMapping("other-namespace", "tenant-b", "shared-agent"),
	}
	mappingClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithIndex(
			&infrastructurev1.HarnessGitopsProjectMapping{},
			projectMappingAgentRefIndexField,
			projectMappingAgentRefIndexValues,
		).
		Build()

	agent := &infrastructurev1.HarnessGitopsAgent{ObjectMeta: metav1.ObjectMeta{
		Name:      "shared-agent",
		Namespace: "tenant-a",
	}}
	got := agentToProjectMappingRequests(mappingClient)(context.Background(), agent)
	want := []reconcile.Request{
		{NamespacedName: client.ObjectKey{Namespace: "tenant-a", Name: "a-first"}},
		{NamespacedName: client.ObjectKey{Namespace: "tenant-a", Name: "z-last"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
}

func TestAgentEventMappingHandlesWrongObjectsAndListErrors(t *testing.T) {
	scheme := relationshipTestScheme(t)
	sentinel := errors.New("list failed")
	mappingClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(
				_ context.Context,
				_ client.WithWatch,
				_ client.ObjectList,
				_ ...client.ListOption,
			) error {
				return sentinel
			},
		}).
		Build()
	mapRequests := agentToProjectMappingRequests(mappingClient)

	if got := mapRequests(context.Background(), &corev1.Secret{}); got != nil {
		t.Fatalf("wrong object requests = %#v, want nil", got)
	}
	agent := &infrastructurev1.HarnessGitopsAgent{ObjectMeta: metav1.ObjectMeta{
		Name:      "shared-agent",
		Namespace: "tenant-a",
	}}
	if got := mapRequests(context.Background(), agent); got != nil {
		t.Fatalf("list-error requests = %#v, want nil", got)
	}
}

func relationshipTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := infrastructurev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add operator scheme: %v", err)
	}
	return scheme
}

func relationshipTestMapping(
	name string,
	namespace string,
	agentName string,
) *infrastructurev1.HarnessGitopsProjectMapping {
	return &infrastructurev1.HarnessGitopsProjectMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: infrastructurev1.HarnessGitopsProjectMappingSpec{
			AgentRef: infrastructurev1.HarnessGitopsAgentReference{
				Name: agentName,
			},
			AppProject: "default",
		},
	}
}
