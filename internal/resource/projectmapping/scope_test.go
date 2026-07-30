package projectmapping

import (
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrastructurev1 "github.com/markoskandylis/harness-gitops-agent-operator/api/v1"
)

const (
	scopeTestNamespace       = "mapping-scope-tests"
	scopeTestAgentName       = "mapping-agent"
	scopeTestAccountID       = "agent-account"
	scopeTestAgentOrgID      = "agent-home-org"
	scopeTestAgentProjectID  = "agent-home-project"
	scopeTestTargetOrgID     = "target-mapping-org"
	scopeTestTargetProjectID = "target-mapping-project"
	scopeTestAppProject      = "argocd-app-project"
)

func TestResolveProjectMappingRequest(t *testing.T) {
	projectAgent := newMappingScopeTestAgent(agentScopeProject)
	projectMapping := newMappingScopeTestMapping()

	projectExplicitAgent := newMappingScopeTestAgent(" project ")
	projectExplicitMapping := newMappingScopeTestMapping()
	projectExplicitMapping.Spec.OrgID = " " + scopeTestAgentOrgID + " "
	projectExplicitMapping.Spec.ProjectID = " " + scopeTestAgentProjectID + " "

	orgAgent := newMappingScopeTestAgent(agentScopeOrg)
	orgAgent.Spec.ProjectId = "agent-project-must-not-leak"
	orgAgent.Spec.ExistingAgentIdentifier = " shared-org-agent "
	orgAgent.Status.AgentIdentifier = ""
	orgMapping := newMappingScopeTestMapping()
	orgMapping.Spec.ProjectID = " " + scopeTestTargetProjectID + " "
	orgMapping.Spec.AutoCreateServiceEnv = true

	accountAgent := newMappingScopeTestAgent(agentScopeAccount)
	accountAgent.Spec.OrgId = "agent-org-must-not-leak"
	accountAgent.Spec.ProjectId = "agent-project-must-not-leak"
	accountMapping := newMappingScopeTestMapping()
	accountMapping.Spec.OrgID = " " + scopeTestTargetOrgID + " "
	accountMapping.Spec.ProjectID = " " + scopeTestTargetProjectID + " "

	tests := []struct {
		name    string
		agent   *infrastructurev1.HarnessGitopsAgent
		mapping *infrastructurev1.HarnessGitopsProjectMapping
		want    ProjectMappingRequest
	}{
		{
			name:    "project scope inherits agent org and project",
			agent:   projectAgent,
			mapping: projectMapping,
			want: ProjectMappingRequest{
				AccountIdentifier: scopeTestAccountID,
				AgentIdentifier:   "registered-agent",
				AgentScope:        agentScopeProject,
				Agent: Scope{
					OrgIdentifier:     scopeTestAgentOrgID,
					ProjectIdentifier: scopeTestAgentProjectID,
				},
				Mapping: Scope{
					OrgIdentifier:     scopeTestAgentOrgID,
					ProjectIdentifier: scopeTestAgentProjectID,
				},
				ArgoProjectName: scopeTestAppProject,
			},
		},
		{
			name:    "project scope accepts equivalent explicit target",
			agent:   projectExplicitAgent,
			mapping: projectExplicitMapping,
			want: ProjectMappingRequest{
				AccountIdentifier: scopeTestAccountID,
				AgentIdentifier:   "registered-agent",
				AgentScope:        agentScopeProject,
				Agent: Scope{
					OrgIdentifier:     scopeTestAgentOrgID,
					ProjectIdentifier: scopeTestAgentProjectID,
				},
				Mapping: Scope{
					OrgIdentifier:     scopeTestAgentOrgID,
					ProjectIdentifier: scopeTestAgentProjectID,
				},
				ArgoProjectName: scopeTestAppProject,
			},
		},
		{
			name:    "org scope uses existing agent and explicit target project",
			agent:   orgAgent,
			mapping: orgMapping,
			want: ProjectMappingRequest{
				AccountIdentifier: scopeTestAccountID,
				AgentIdentifier:   "shared-org-agent",
				AgentScope:        agentScopeOrg,
				Agent: Scope{
					OrgIdentifier: scopeTestAgentOrgID,
				},
				Mapping: Scope{
					OrgIdentifier:     scopeTestAgentOrgID,
					ProjectIdentifier: scopeTestTargetProjectID,
				},
				ArgoProjectName:      scopeTestAppProject,
				AutoCreateServiceEnv: true,
			},
		},
		{
			name:    "account scope keeps agent and target scopes separate",
			agent:   accountAgent,
			mapping: accountMapping,
			want: ProjectMappingRequest{
				AccountIdentifier: scopeTestAccountID,
				AgentIdentifier:   "registered-agent",
				AgentScope:        agentScopeAccount,
				Agent:             Scope{},
				Mapping: Scope{
					OrgIdentifier:     scopeTestTargetOrgID,
					ProjectIdentifier: scopeTestTargetProjectID,
				},
				ArgoProjectName: scopeTestAppProject,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveProjectMappingRequest(test.agent, test.mapping)
			if err != nil {
				t.Fatalf("resolve mapping request: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("request = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestResolveProjectMappingRequestRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*infrastructurev1.HarnessGitopsAgent, *infrastructurev1.HarnessGitopsProjectMapping)
		want   string
	}{
		{
			name: "different namespace",
			mutate: func(_ *infrastructurev1.HarnessGitopsAgent, mapping *infrastructurev1.HarnessGitopsProjectMapping) {
				mapping.Namespace = "another-namespace"
			},
			want: "same namespace",
		},
		{
			name: "different agent reference",
			mutate: func(_ *infrastructurev1.HarnessGitopsAgent, mapping *infrastructurev1.HarnessGitopsProjectMapping) {
				mapping.Spec.AgentRef.Name = "another-agent"
			},
			want: "does not match Agent",
		},
		{
			name: "managed agent is not registered",
			mutate: func(agent *infrastructurev1.HarnessGitopsAgent, _ *infrastructurev1.HarnessGitopsProjectMapping) {
				agent.Status.AgentIdentifier = ""
			},
			want: "is not registered",
		},
		{
			name: "account is empty",
			mutate: func(agent *infrastructurev1.HarnessGitopsAgent, _ *infrastructurev1.HarnessGitopsProjectMapping) {
				agent.Spec.AccountId = " "
			},
			want: "empty spec.accountId",
		},
		{
			name: "app project is empty",
			mutate: func(_ *infrastructurev1.HarnessGitopsAgent, mapping *infrastructurev1.HarnessGitopsProjectMapping) {
				mapping.Spec.AppProject = " "
			},
			want: "spec.appProject must not be empty",
		},
		{
			name: "unsupported scope",
			mutate: func(agent *infrastructurev1.HarnessGitopsAgent, _ *infrastructurev1.HarnessGitopsProjectMapping) {
				agent.Spec.Scope = "ENVIRONMENT"
			},
			want: "unsupported Agent scope",
		},
		{
			name: "project agent org is empty",
			mutate: func(agent *infrastructurev1.HarnessGitopsAgent, _ *infrastructurev1.HarnessGitopsProjectMapping) {
				agent.Spec.OrgId = ""
			},
			want: "PROJECT-scoped Agent requires",
		},
		{
			name: "project target org conflicts",
			mutate: func(_ *infrastructurev1.HarnessGitopsAgent, mapping *infrastructurev1.HarnessGitopsProjectMapping) {
				mapping.Spec.OrgID = scopeTestTargetOrgID
			},
			want: "must be empty or match the PROJECT-scoped Agent org",
		},
		{
			name: "project target project conflicts",
			mutate: func(_ *infrastructurev1.HarnessGitopsAgent, mapping *infrastructurev1.HarnessGitopsProjectMapping) {
				mapping.Spec.ProjectID = scopeTestTargetProjectID
			},
			want: "must be empty or match the PROJECT-scoped Agent project",
		},
		{
			name: "org target project is empty",
			mutate: func(agent *infrastructurev1.HarnessGitopsAgent, _ *infrastructurev1.HarnessGitopsProjectMapping) {
				agent.Spec.Scope = agentScopeOrg
			},
			want: "spec.projectId is required for an ORG-scoped Agent",
		},
		{
			name: "org target org conflicts",
			mutate: func(agent *infrastructurev1.HarnessGitopsAgent, mapping *infrastructurev1.HarnessGitopsProjectMapping) {
				agent.Spec.Scope = agentScopeOrg
				mapping.Spec.OrgID = scopeTestTargetOrgID
				mapping.Spec.ProjectID = scopeTestTargetProjectID
			},
			want: "must be empty or match the ORG-scoped Agent org",
		},
		{
			name: "account target org is empty",
			mutate: func(agent *infrastructurev1.HarnessGitopsAgent, mapping *infrastructurev1.HarnessGitopsProjectMapping) {
				agent.Spec.Scope = agentScopeAccount
				mapping.Spec.ProjectID = scopeTestTargetProjectID
			},
			want: "spec.orgId and spec.projectId are required",
		},
		{
			name: "account target project is empty",
			mutate: func(agent *infrastructurev1.HarnessGitopsAgent, mapping *infrastructurev1.HarnessGitopsProjectMapping) {
				agent.Spec.Scope = agentScopeAccount
				mapping.Spec.OrgID = scopeTestTargetOrgID
			},
			want: "spec.orgId and spec.projectId are required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := newMappingScopeTestAgent(agentScopeProject)
			mapping := newMappingScopeTestMapping()
			test.mutate(agent, mapping)

			_, err := resolveProjectMappingRequest(agent, mapping)
			if err == nil {
				t.Fatal("expected resolution to fail")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestResolveProjectMappingRequestRejectsNilObjects(t *testing.T) {
	agent := newMappingScopeTestAgent(agentScopeProject)
	mapping := newMappingScopeTestMapping()

	if _, err := resolveProjectMappingRequest(nil, mapping); err == nil {
		t.Fatal("expected a nil Agent to fail")
	}
	if _, err := resolveProjectMappingRequest(agent, nil); err == nil {
		t.Fatal("expected a nil Mapping to fail")
	}
}

func newMappingScopeTestAgent(scope string) *infrastructurev1.HarnessGitopsAgent {
	return &infrastructurev1.HarnessGitopsAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scopeTestAgentName,
			Namespace: scopeTestNamespace,
		},
		Spec: infrastructurev1.HarnessGitopsAgentSpec{
			AccountId: scopeTestAccountID,
			OrgId:     scopeTestAgentOrgID,
			ProjectId: scopeTestAgentProjectID,
			Scope:     scope,
		},
		Status: infrastructurev1.HarnessGitopsAgentStatus{
			AgentIdentifier: " registered-agent ",
		},
	}
}

func newMappingScopeTestMapping() *infrastructurev1.HarnessGitopsProjectMapping {
	return &infrastructurev1.HarnessGitopsProjectMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "project-mapping",
			Namespace: scopeTestNamespace,
		},
		Spec: infrastructurev1.HarnessGitopsProjectMappingSpec{
			AgentRef: infrastructurev1.HarnessGitopsAgentReference{
				Name: scopeTestAgentName,
			},
			AppProject: " " + scopeTestAppProject + " ",
		},
	}
}
