/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProjectMappingSpec defines explicit Harness <-> ArgoCD AppProject mapping input.
type ProjectMappingSpec struct {
	// OrgId is the Harness organization that owns ProjectId. It is a property of
	// the mapping, not of the agent: an ACCOUNT-scoped agent exists to serve
	// projects in many orgs and therefore has no org of its own (Spec.OrgId is
	// empty by design), so the org has to be supplied per mapping.
	// When empty the controller falls back to Spec.OrgId, which keeps ORG- and
	// PROJECT-scoped agents working without setting this field.
	// +optional
	OrgId string `json:"orgId,omitempty"`

	// ProjectId is the Harness project identifier to map to.
	// +optional
	ProjectId string `json:"projectId,omitempty"`

	// AppProject is the ArgoCD AppProject name to map.
	// Intentionally exported as AppProject to match the requested CR manifest contract.
	// +optional
	AppProject string `json:"AppProject,omitempty"`
}

// ResourceOwnership records whether this controller may manage a remote
// Harness resource lifecycle.
// +kubebuilder:validation:Enum=Managed;External
type ResourceOwnership string

const (
	// OwnershipManaged means this controller successfully created the resource.
	OwnershipManaged ResourceOwnership = "Managed"
	// OwnershipExternal means the resource existed before this controller saw it.
	OwnershipExternal ResourceOwnership = "External"
)

// HarnessGitopsAgentSpec defines the desired state of HarnessGitopsAgent.
// Harness identity fields are immutable because changing them would leave the
// previously registered remote agent outside the controller's lifecycle.
// +kubebuilder:validation:XValidation:rule="self.name == oldSelf.name && self.operator == oldSelf.operator && self.identifier == oldSelf.identifier && self.accountId == oldSelf.accountId && self.type == oldSelf.type && self.scope == oldSelf.scope && has(self.orgId) == has(oldSelf.orgId) && (!has(self.orgId) || self.orgId == oldSelf.orgId) && has(self.projectId) == has(oldSelf.projectId) && (!has(self.projectId) || self.projectId == oldSelf.projectId) && has(self.existingAgentIdentifier) == has(oldSelf.existingAgentIdentifier) && (!has(self.existingAgentIdentifier) || self.existingAgentIdentifier == oldSelf.existingAgentIdentifier)",message="Harness agent identity is immutable; replace the resource instead"
type HarnessGitopsAgentSpec struct {
	// Name is the name of the Harness GitOps Agent
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Type of GitOps Operator ("ARGO", "FLAMINGO")
	// +kubebuilder:validation:Required
	Operator string `json:"operator,omitempty"`

	// Identifier is the unique identifier of the Harness GitOps Agent
	// +kubebuilder:validation:Required
	Identifier string `json:"identifier,omitempty"`

	// AccountId is the Harness Account Identifier
	// +kubebuilder:validation:Required
	AccountId string `json:"accountId"`

	// OrgId is the Harness Organization Identifier
	// +kubebuilder:validation:optional
	OrgId string `json:"orgId,omitempty"`

	// ProjectId is the Harness Project Identifier
	// +kubebuilder:validation:optional
	ProjectId string `json:"projectId,omitempty"`

	// Type of agent (e.g., "MANAGED_ARGO_PROVIDER")
	// +kubebuilder:validation:Enum=MANAGED_ARGO_PROVIDER;CONNECTED_ARGO_PROVIDER
	// +kubebuilder:default:="MANAGED_ARGO_PROVIDER"
	Type string `json:"type,omitempty"`

	// Scope of the agent (e.g., "ACCOUNT", "ORG", "PROJECT")
	// +kubebuilder:default:="PROJECT"
	Scope string `json:"scope,omitempty"`

	// ExistingAgentIdentifier, if set, skips agent creation and reuses this already-running
	// agent for project mapping. TokenSecretRef is not required in this mode.
	// +optional
	ExistingAgentIdentifier string `json:"existingAgentIdentifier,omitempty"`

	// ProjectMapping configures explicit Harness project <-> ArgoCD AppProject mapping.
	// Optional, and supported for ACCOUNT, ORG, or PROJECT scope.
	// +optional
	ProjectMapping *ProjectMappingSpec `json:"projectMapping,omitempty"`

	// ApiKeySecretRef is the name of the Secret containing the Harness API Key.
	// Key inside secret must be "api_key".
	// +kubebuilder:validation:Required
	ApiKeySecretRef string `json:"apiKeySecretRef"`

	// TokenSecretRef is the name of the Secret where the generated Agent Token will be stored.
	// The controller writes GITOPS_AGENT_TOKEN into this secret.
	// Not required when ExistingAgentIdentifier is set.
	// +optional
	TokenSecretRef string `json:"tokenSecretRef,omitempty"`
}

// HarnessGitopsAgentStatus defines the observed state of HarnessGitopsAgent.
type HarnessGitopsAgentStatus struct {
	// AgentIdentifier is the ID returned by Harness after agent registration.
	AgentIdentifier string `json:"agentIdentifier,omitempty"`

	// AgentOwnership determines whether finalization may delete the remote agent.
	// Empty means ownership was not proven and is handled as external.
	// +optional
	AgentOwnership ResourceOwnership `json:"agentOwnership,omitempty"`

	// ArgoProjectId is the mapped ArgoCD AppProject name.
	// Used as the `project:` field in ApplicationSets and Applications.
	ArgoProjectId string `json:"argoProjectId,omitempty"`

	// ArgoProjectMappingId is the identifier returned by the most recent successful
	// Harness mapping verification. It is an observation, not an idempotency guard.
	// +optional
	ArgoProjectMappingId string `json:"argoProjectMappingId,omitempty"`

	// ArgoProjectMappingOwnership determines whether finalization may delete the
	// remote mapping. Empty is treated conservatively as external.
	// +optional
	ArgoProjectMappingOwnership ResourceOwnership `json:"argoProjectMappingOwnership,omitempty"`

	// ArgoProjectMappingOrgId is the resolved Harness organization of the most
	// recently verified mapping.
	// +optional
	ArgoProjectMappingOrgId string `json:"argoProjectMappingOrgId,omitempty"`

	// ArgoProjectMappingProjectId is the resolved Harness project of the most
	// recently verified mapping.
	// +optional
	ArgoProjectMappingProjectId string `json:"argoProjectMappingProjectId,omitempty"`

	// Conditions store the detailed state transitions.
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.status.agentIdentifier`
// +kubebuilder:printcolumn:name="ArgoProject",type=string,JSONPath=`.status.argoProjectId`

// HarnessGitopsAgent is the Schema for the harnessgitopsagents API
type HarnessGitopsAgent struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of HarnessGitopsAgent
	// +required
	Spec HarnessGitopsAgentSpec `json:"spec"`

	// status defines the observed state of HarnessGitopsAgent
	// +optional
	Status HarnessGitopsAgentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// HarnessGitopsAgentList contains a list of HarnessGitopsAgent
type HarnessGitopsAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HarnessGitopsAgent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HarnessGitopsAgent{}, &HarnessGitopsAgentList{})
}
