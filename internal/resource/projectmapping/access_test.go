package projectmapping

import (
	"testing"

	resourceutil "github.com/markoskandylis/harness-gitops-agent-operator/internal/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func mappingPolicyForObjects(t *testing.T, objects ...client.Object) resourceutil.NamespacePolicy {
	t.Helper()
	var namespaces []string
	for _, object := range objects {
		if object.GetNamespace() != "" {
			namespaces = append(namespaces, object.GetNamespace())
		}
	}
	policy, err := resourceutil.NewNamespacePolicy(namespaces)
	if err != nil {
		t.Fatalf("configure test namespace policy: %v", err)
	}
	return policy
}
