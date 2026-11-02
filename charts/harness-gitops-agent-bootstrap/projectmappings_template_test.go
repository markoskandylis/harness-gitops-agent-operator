package bootstrap

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestAccountScopeRendersManyMappingsWithOneAgentReference(t *testing.T) {
	requireBootstrapHelm(t)
	args := bootstrapValuesArgs(true)
	args = append(args,
		"--set-string", "harnessAgent.spec.scope=ACCOUNT",
		"--set-string", "gitopsAgent.harness.identity.orgIdentifier=",
		"--set-string", "gitopsAgent.harness.identity.projectIdentifier=",
		"--set-string", "projectMappings[0].name=payments",
		"--set-string", "projectMappings[0].appProject=payments",
		"--set-string", "projectMappings[0].orgId=platform",
		"--set-string", "projectMappings[0].projectId=payments-project",
		"--set-string", "projectMappings[1].name=orders",
		"--set-string", "projectMappings[1].appProject=orders",
		"--set-string", "projectMappings[1].orgId=commerce",
		"--set-string", "projectMappings[1].projectId=orders-project",
		"--set", "projectMappings[1].autoCreateServiceEnv=true",
		"--set-string", "projectMappings[1].adoptMappingId=existing-orders-mapping",
	)

	objects := decodeBootstrapYAML(t, renderBootstrap(t, args...))
	agent := findBootstrapObject(t, objects, "HarnessGitopsAgent", "test-agent")
	if _, found, err := unstructured.NestedFieldNoCopy(agent, "spec", "projectMapping"); err != nil || found {
		t.Fatalf("Agent must not contain spec.projectMapping: found=%t err=%v", found, err)
	}

	mappings := objectsByKind(objects, "HarnessGitopsProjectMapping")
	if len(mappings) != 2 {
		t.Fatalf("rendered %d Mapping CRs, want 2", len(mappings))
	}
	for _, mapping := range mappings {
		agentRef, _, err := unstructured.NestedString(mapping, "spec", "agentRef", "name")
		if err != nil {
			t.Fatalf("read Mapping agentRef: %v", err)
		}
		if agentRef != "test-agent" {
			t.Fatalf("Mapping agentRef.name = %q, want test-agent", agentRef)
		}
	}

	orders := findBootstrapObject(t, objects, "HarnessGitopsProjectMapping", "orders")
	assertNestedString(t, orders, "existing-orders-mapping", "spec", "adoptMappingId")
	autoCreate, _, err := unstructured.NestedBool(orders, "spec", "autoCreateServiceEnv")
	if err != nil {
		t.Fatalf("read autoCreateServiceEnv: %v", err)
	}
	if !autoCreate {
		t.Fatal("orders autoCreateServiceEnv = false, want true")
	}
}

func TestProjectScopeMappingsMayInheritAgentTarget(t *testing.T) {
	requireBootstrapHelm(t)
	args := bootstrapValuesArgs(true)
	args = append(args,
		"--set-string", "projectMappings[0].name=default",
		"--set-string", "projectMappings[0].appProject=default",
	)
	objects := decodeBootstrapYAML(t, renderBootstrap(t, args...))
	mapping := findBootstrapObject(t, objects, "HarnessGitopsProjectMapping", "default")

	for _, field := range []string{"orgId", "projectId", "adoptMappingId"} {
		if _, found, err := unstructured.NestedFieldNoCopy(mapping, "spec", field); err != nil || found {
			t.Fatalf("inherited spec.%s must be omitted: found=%t err=%v", field, found, err)
		}
	}
}

func TestProjectMappingRenderValidation(t *testing.T) {
	requireBootstrapHelm(t)
	tests := []struct {
		name      string
		wantError string
		args      []string
	}{
		{
			name:      "removed embedded key",
			wantError: "harnessAgent.spec.projectMapping was removed",
			args: []string{
				"--set-string", "harnessAgent.spec.projectMapping.AppProject=default",
			},
		},
		{
			name:      "blank name",
			wantError: "projectMappings[0].name is required",
			args: []string{
				"--set-string", "projectMappings[0].name= ",
				"--set-string", "projectMappings[0].appProject=default",
			},
		},
		{
			name:      "blank AppProject",
			wantError: "projectMappings[0].appProject is required",
			args: []string{
				"--set-string", "projectMappings[0].name=default",
				"--set-string", "projectMappings[0].appProject= ",
			},
		},
		{
			name:      "duplicate names",
			wantError: "projectMappings contains duplicate name \"duplicate\"",
			args: []string{
				"--set-string", "projectMappings[0].name=duplicate",
				"--set-string", "projectMappings[0].appProject=first",
				"--set-string", "projectMappings[1].name=duplicate",
				"--set-string", "projectMappings[1].appProject=second",
			},
		},
		{
			name:      "ACCOUNT missing org",
			wantError: "projectMappings[0].orgId is required for an ACCOUNT-scoped Agent",
			args: []string{
				"--set-string", "harnessAgent.spec.scope=ACCOUNT",
				"--set-string", "projectMappings[0].name=default",
				"--set-string", "projectMappings[0].appProject=default",
				"--set-string", "projectMappings[0].projectId=target-project",
			},
		},
		{
			name:      "ACCOUNT missing project",
			wantError: "projectMappings[0].projectId is required for an ACCOUNT-scoped Agent",
			args: []string{
				"--set-string", "harnessAgent.spec.scope=ACCOUNT",
				"--set-string", "projectMappings[0].name=default",
				"--set-string", "projectMappings[0].appProject=default",
				"--set-string", "projectMappings[0].orgId=target-org",
			},
		},
		{
			name:      "ORG missing project",
			wantError: "projectMappings[0].projectId is required for an ORG-scoped Agent",
			args: []string{
				"--set-string", "harnessAgent.spec.scope=ORG",
				"--set-string", "projectMappings[0].name=default",
				"--set-string", "projectMappings[0].appProject=default",
			},
		},
		{
			name:      "ORG crosses org boundary",
			wantError: "must be omitted or match the Agent org \"agent-org\"",
			args: []string{
				"--set-string", "harnessAgent.spec.scope=ORG",
				"--set-string", "projectMappings[0].name=default",
				"--set-string", "projectMappings[0].appProject=default",
				"--set-string", "projectMappings[0].orgId=other-org",
				"--set-string", "projectMappings[0].projectId=target-project",
			},
		},
		{
			name:      "PROJECT crosses project boundary",
			wantError: "must be omitted or match the PROJECT-scoped Agent project \"agent-project\"",
			args: []string{
				"--set-string", "projectMappings[0].name=default",
				"--set-string", "projectMappings[0].appProject=default",
				"--set-string", "projectMappings[0].projectId=other-project",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append(bootstrapValuesArgs(true), test.args...)
			output, err := renderBootstrapResult(args...)
			if err == nil {
				t.Fatalf("helm template succeeded, want error containing %q", test.wantError)
			}
			if !strings.Contains(string(output), test.wantError) {
				t.Fatalf("error does not contain %q:\n%s", test.wantError, output)
			}
		})
	}
}

func TestAppProjectRequiresAPIAndRendersWhenAvailable(t *testing.T) {
	requireBootstrapHelm(t)
	args := bootstrapValuesArgs(false)
	args = append(args, "--set", "appProject.enabled=true")

	output, err := renderBootstrapResult(args...)
	if err == nil {
		t.Fatal("AppProject rendered without its API being available")
	}
	if !strings.Contains(string(output), "appProject.enabled requires the AppProject CRD to exist") {
		t.Fatalf("unexpected AppProject validation error:\n%s", output)
	}

	args = append(args, "--api-versions", "argoproj.io/v1alpha1/AppProject")
	objects := decodeBootstrapYAML(t, renderBootstrap(t, args...))
	appProject := findBootstrapObject(t, objects, "AppProject", "default")
	assertNestedString(t, appProject, "hga-test", "metadata", "namespace")
}

func TestAppProjectCannotRenderOutsideTheAgentNamespace(t *testing.T) {
	requireBootstrapHelm(t)
	args := bootstrapValuesArgs(false)
	args = append(args,
		"--set",
		"appProject.enabled=true",
		"--set-string",
		"appProject.namespace=other",
		"--api-versions",
		"argoproj.io/v1alpha1/AppProject",
	)

	output, err := renderBootstrapResult(args...)
	if err == nil {
		t.Fatal("AppProject namespace override was accepted")
	}
	if !strings.Contains(string(output), "additional properties 'namespace' not allowed") {
		t.Fatalf("unexpected AppProject namespace validation error:\n%s", output)
	}
}

func TestDefaultFlowRendersAgentAndRuntimeTogether(t *testing.T) {
	requireBootstrapHelm(t)
	objects := decodeBootstrapYAML(t, renderBootstrap(t, bootstrapValuesArgs(false)...))
	findBootstrapObject(t, objects, "HarnessGitopsAgent", "test-agent")
	if len(objectsByKind(objects, "Deployment")) == 0 {
		t.Fatal("default values did not render the Agent runtime")
	}
}

func bootstrapValuesArgs(disableRuntime bool) []string {
	args := []string{
		"--set-string", "harnessAgent.metadata.name=test-agent",
		"--set-string", "harnessAgent.spec.tokenSecretRef=test-token",
		"--set-string", "gitopsAgent.agent.existingSecrets.agentToken=test-token",
		"--set-string", "gitopsAgent.harness.identity.accountIdentifier=account",
		"--set-string", "gitopsAgent.harness.identity.orgIdentifier=agent-org",
		"--set-string", "gitopsAgent.harness.identity.projectIdentifier=agent-project",
		"--set-string", "gitopsAgent.harness.identity.agentIdentifier=test_agent",
		"--set", "gitopsAgent.argo-cd.crds.install=false",
	}
	if disableRuntime {
		args = append(args, "--set", "gitopsAgent.enabled=false")
	}
	return args
}

func requireBootstrapHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is required for chart render tests")
	}
}

func renderBootstrap(t *testing.T, args ...string) []byte {
	t.Helper()
	output, err := renderBootstrapResult(args...)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, output)
	}
	return output
}

func renderBootstrapResult(args ...string) ([]byte, error) {
	commandArgs := []string{"template", "chart-test", ".", "--namespace", "hga-test"}
	commandArgs = append(commandArgs, args...)
	return exec.Command("helm", commandArgs...).CombinedOutput()
}

func decodeBootstrapYAML(t *testing.T, content []byte) []map[string]interface{} {
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

func objectsByKind(objects []map[string]interface{}, kind string) []map[string]interface{} {
	var matches []map[string]interface{}
	for _, object := range objects {
		if object["kind"] == kind {
			matches = append(matches, object)
		}
	}
	return matches
}

func findBootstrapObject(t *testing.T, objects []map[string]interface{}, kind, name string) map[string]interface{} {
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

func assertNestedString(t *testing.T, object map[string]interface{}, want string, fields ...string) {
	t.Helper()
	got, found, err := unstructured.NestedString(object, fields...)
	if err != nil {
		t.Fatalf("read %s: %v", strings.Join(fields, "."), err)
	}
	if !found || got != want {
		t.Fatalf("%s = %q (found=%t), want %q", strings.Join(fields, "."), got, found, want)
	}
}
