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

// HarnessGitopsAgentSpec defines the desired state of HarnessGitopsAgent
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

	// ArgoProjectName is the name of the existing in-cluster ArgoCD AppProject to map
	// to the Harness project in spec.projectId.
	// Required when ExistingAgentIdentifier is set.
	// Required when scope is ORG or ACCOUNT and projectId is set (used to create the primary mapping).
	// +optional
	ArgoProjectName string `json:"argoProjectName,omitempty"`

	// ApiKeySecretRef is the name of the Secret containing the Harness API Key.
	// Key inside secret must be "api_key".
	// +kubebuilder:validation:Required
	ApiKeySecretRef string `json:"apiKeySecretRef"`

	// TokenSecretRef is the name of the Secret where the generated Agent Token will be stored.
	// The controller writes GITOPS_AGENT_TOKEN and ARGO_PROJECT_ID into this secret.
	// Not required when ExistingAgentIdentifier is set.
	// +optional
	TokenSecretRef string `json:"tokenSecretRef,omitempty"`
}

// HarnessGitopsAgentStatus defines the observed state of HarnessGitopsAgent.
type HarnessGitopsAgentStatus struct {
	// AgentIdentifier is the ID returned by Harness after agent registration.
	AgentIdentifier string `json:"agentIdentifier,omitempty"`

	// ArgoProjectId is the ArgoCD AppProject name (random 8-char ID) that Harness
	// creates and maps to this agent's Harness project after cluster registration.
	// Used as the `project:` field in ApplicationSets and Applications.
	ArgoProjectId string `json:"argoProjectId,omitempty"`

	// ArgoProjectMappingId is the identifier returned by Harness after the AppProject
	// mapping is created via AppProjectMappingServiceCreateV2. Used as an idempotency guard.
	// +optional
	ArgoProjectMappingId string `json:"argoProjectMappingId,omitempty"`

	// Conditions store the detailed state transitions.
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.status.agentIdentifier`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.status.clusterIdentifier`
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
