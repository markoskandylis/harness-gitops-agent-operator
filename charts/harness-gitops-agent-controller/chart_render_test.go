package controllerchart

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	agentCRDName   = "harnessgitopsagents.infrastructure.kandylis.co.uk"
	mappingCRDName = "harnessgitopsprojectmappings.infrastructure.kandylis.co.uk"
)

func TestRenderedCRDsMatchGeneratedSpecs(t *testing.T) {
	rendered := decodeYAMLDocuments(t, helmTemplate(t))

	for _, test := range []struct {
		name      string
		generated string
		shortName string
	}{
		{
			name:      agentCRDName,
			generated: "../../config/crd/bases/infrastructure.kandylis.co.uk_harnessgitopsagents.yaml",
			shortName: "hga",
		},
		{
			name:      mappingCRDName,
			generated: "../../config/crd/bases/infrastructure.kandylis.co.uk_harnessgitopsprojectmappings.yaml",
			shortName: "hgapm",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual := findObject(t, rendered, "CustomResourceDefinition", test.name)
			expected := decodeYAMLFile(t, test.generated)

			actualSpec, _, err := unstructured.NestedMap(actual, "spec")
			if err != nil {
				t.Fatalf("read rendered CRD spec: %v", err)
			}
			expectedSpec, _, err := unstructured.NestedMap(expected, "spec")
			if err != nil {
				t.Fatalf("read generated CRD spec: %v", err)
			}
			if !reflect.DeepEqual(actualSpec, expectedSpec) {
				t.Fatalf("rendered %s spec drifted from %s; refresh the chart CRD from make manifests", test.name, test.generated)
			}

			shortNames, _, err := unstructured.NestedStringSlice(actual, "spec", "names", "shortNames")
			if err != nil {
				t.Fatalf("read rendered CRD shortNames: %v", err)
			}
			if !reflect.DeepEqual(shortNames, []string{test.shortName}) {
				t.Fatalf("shortNames = %v, want [%s]", shortNames, test.shortName)
			}

			keep, _, err := unstructured.NestedString(actual, "metadata", "annotations", "helm.sh/resource-policy")
			if err != nil {
				t.Fatalf("read Helm keep annotation: %v", err)
			}
			if keep != "keep" {
				t.Fatalf("helm.sh/resource-policy = %q, want keep", keep)
			}
		})
	}
}

func TestRenderedClusterRoleMatchesGeneratedRules(t *testing.T) {
	rendered := decodeYAMLDocuments(t, helmTemplate(t))
	actual := findObjectByKindSuffix(t, rendered, "ClusterRole", "-manager")
	expected := decodeYAMLFile(t, "../../config/rbac/role.yaml")

	actualRules, _, err := unstructured.NestedSlice(actual, "rules")
	if err != nil {
		t.Fatalf("read rendered ClusterRole rules: %v", err)
	}
	expectedRules, _, err := unstructured.NestedSlice(expected, "rules")
	if err != nil {
		t.Fatalf("read generated ClusterRole rules: %v", err)
	}
	if !reflect.DeepEqual(actualRules, expectedRules) {
		t.Fatal("rendered ClusterRole rules drifted from config/rbac/role.yaml")
	}
}

func TestCRDLifecycleFlagsApplyToBothCRDs(t *testing.T) {
	disabled := decodeYAMLDocuments(t, helmTemplate(t, "--set", "crds.enabled=false"))
	for _, object := range disabled {
		if object["kind"] == "CustomResourceDefinition" {
			t.Fatalf("crds.enabled=false rendered CRD %v", object["metadata"])
		}
	}

	withoutKeep := decodeYAMLDocuments(t, helmTemplate(t, "--set", "crds.keep=false"))
	for _, name := range []string{agentCRDName, mappingCRDName} {
		crd := findObject(t, withoutKeep, "CustomResourceDefinition", name)
		annotation, found, err := unstructured.NestedString(crd, "metadata", "annotations", "helm.sh/resource-policy")
		if err != nil {
			t.Fatalf("read %s annotations: %v", name, err)
		}
		if found {
			t.Fatalf("%s retained helm.sh/resource-policy=%q with crds.keep=false", name, annotation)
		}
	}
}

func TestAPIKeySecretNamespaceArgument(t *testing.T) {
	for _, test := range []struct {
		name      string
		extraArgs []string
		want      string
	}{
		{
			name: "defaults to release namespace",
			want: "--api-key-secret-namespace=hga-system",
		},
		{
			name:      "preserves explicit override",
			extraArgs: []string{"--set", "manager.apiKeySecretNamespace=credential-system"},
			want:      "--api-key-secret-namespace=credential-system",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered := decodeYAMLDocuments(t, helmTemplate(t, test.extraArgs...))
			deployment := findObjectByKindSuffix(t, rendered, "Deployment", "-harness-gitops-agent-controller")
			containers, found, err := unstructured.NestedSlice(
				deployment,
				"spec", "template", "spec", "containers",
			)
			if err != nil {
				t.Fatalf("read controller containers: %v", err)
			}
			if !found || len(containers) != 1 {
				t.Fatalf("controller containers = %v, want exactly one", containers)
			}
			container, ok := containers[0].(map[string]interface{})
			if !ok {
				t.Fatalf("controller container has type %T, want map", containers[0])
			}
			args, found, err := unstructured.NestedStringSlice(container, "args")
			if err != nil {
				t.Fatalf("read controller args: %v", err)
			}
			if !found {
				t.Fatal("controller args not found")
			}

			var namespaceArgs []string
			for _, arg := range args {
				if strings.HasPrefix(arg, "--api-key-secret-namespace=") {
					namespaceArgs = append(namespaceArgs, arg)
				}
			}
			if !reflect.DeepEqual(namespaceArgs, []string{test.want}) {
				t.Fatalf("API key namespace args = %v, want [%s]", namespaceArgs, test.want)
			}
		})
	}
}

func helmTemplate(t *testing.T, extraArgs ...string) []byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart render tests")
	}
	args := []string{"template", "chart-test", ".", "--namespace", "hga-system"}
	args = append(args, extraArgs...)
	output, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, output)
	}
	return output
}

func decodeYAMLFile(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	documents := decodeYAMLDocuments(t, content)
	if len(documents) != 1 {
		t.Fatalf("%s contains %d YAML documents, want 1", path, len(documents))
	}
	return documents[0]
}

func decodeYAMLDocuments(t *testing.T, content []byte) []map[string]interface{} {
	t.Helper()
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096)
	var documents []map[string]interface{}
	for {
		var object map[string]interface{}
		err := decoder.Decode(&object)
		if err == io.EOF {
			return documents
		}
		if err != nil {
			t.Fatalf("decode YAML: %v", err)
		}
		if len(object) != 0 {
			documents = append(documents, object)
		}
	}
}

func findObject(t *testing.T, objects []map[string]interface{}, kind, name string) map[string]interface{} {
	t.Helper()
	for _, object := range objects {
		if object["kind"] != kind {
			continue
		}
		objectName, _, _ := unstructured.NestedString(object, "metadata", "name")
		if objectName == name {
			return object
		}
	}
	t.Fatalf("%s %q not found", kind, name)
	return nil
}

func findObjectByKindSuffix(
	t *testing.T,
	objects []map[string]interface{},
	kind string,
	suffix string,
) map[string]interface{} {
	t.Helper()
	for _, object := range objects {
		if object["kind"] != kind {
			continue
		}
		name, _, _ := unstructured.NestedString(object, "metadata", "name")
		if strings.HasSuffix(name, suffix) {
			return object
		}
	}
	t.Fatalf("%s with name suffix %q not found", kind, suffix)
	return nil
}
