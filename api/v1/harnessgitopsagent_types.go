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

// ClusterRegistrationSpec defines how the controller registers the cluster
// that this agent runs on (or connects to) with the Harness GitOps Clusters API.
// Registering a cluster is what triggers Harness to create the ArgoCD AppProject
// mapping for the agent's Harness project.
type ClusterRegistrationSpec struct {
	// Enabled controls whether the controller registers a cluster after agent creation.
	// Defaults to false. Set to true to trigger AppProject creation automatically.
	// +kubebuilder:default:=false
	Enabled bool `json:"enabled"`

	// Server is the Kubernetes API server URL to register.
	// Defaults to "https://kubernetes.default.svc" (in-cluster).
	// For remote clusters use the external API server URL.
	// +kubebuilder:default:="https://kubernetes.default.svc"
	// +optional
	Server string `json:"server,omitempty"`

	// Name is the display name for this cluster in Harness.
	// Defaults to the HarnessGitopsAgent CR name if not set.
	// +optional
	Name string `json:"name,omitempty"`

	// ConnectionType determines the authentication method.
	//   IN_CLUSTER      — agent runs inside the cluster being registered (default).
	//                     Uses https://kubernetes.default.svc, no credentials needed.
	//   SERVICE_ACCOUNT — remote cluster; requires CredentialsRef with bearerToken + caData.
	// +kubebuilder:validation:Enum=IN_CLUSTER;SERVICE_ACCOUNT
	// +kubebuilder:default:="IN_CLUSTER"
	// +optional
	ConnectionType string `json:"connectionType,omitempty"`

	// CredentialsRef is the name of a Secret in the same namespace containing
	// credentials for SERVICE_ACCOUNT connection type.
	// Supported keys:
	//   bearerToken — Kubernetes service account token (required for SERVICE_ACCOUNT)
	//   caData      — base64-encoded PEM CA certificate bundle
	//   insecure    — "true" to skip TLS verification (local/dev only)
	// +optional
	CredentialsRef string `json:"credentialsRef,omitempty"`
}

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
	// +kubebuilder:validation:Required
	OrgId string `json:"orgId"`

	// ProjectId is the Harness Project Identifier
	// +kubebuilder:validation:Required
	ProjectId string `json:"projectId"`

	// Type of agent (e.g., "MANAGED_ARGO_PROVIDER")
	// +kubebuilder:default:="KUBERNETES"
	Type string `json:"type,omitempty"`

	// Scope of the agent (e.g., "ACCOUNT", "ORG", "PROJECT")
	// +kubebuilder:default:="PROJECT"
	Scope string `json:"scope,omitempty"`

	// ApiKeySecretRef is the name of the Secret containing the Harness API Key.
	// Key inside secret must be "api_key".
	// +kubebuilder:validation:Required
	ApiKeySecretRef string `json:"apiKeySecretRef"`

	// TokenSecretRef is the name of the Secret where the generated Agent Token will be stored.
	// The controller writes GITOPS_AGENT_TOKEN and ARGO_PROJECT_ID into this secret.
	// +kubebuilder:validation:Required
	TokenSecretRef string `json:"tokenSecretRef"`

	// ClusterRegistration controls whether the controller registers a cluster with
	// Harness after agent creation. This is required to trigger AppProject creation,
	// which makes the agent and its Applications visible in the Harness UI.
	// +optional
	ClusterRegistration *ClusterRegistrationSpec `json:"clusterRegistration,omitempty"`
}

// HarnessGitopsAgentStatus defines the observed state of HarnessGitopsAgent.
type HarnessGitopsAgentStatus struct {
	// AgentIdentifier is the ID returned by Harness after agent registration.
	AgentIdentifier string `json:"agentIdentifier,omitempty"`

	// ClusterIdentifier is the Harness identifier of the cluster registered via
	// ClusterRegistration. Populated after successful cluster registration.
	ClusterIdentifier string `json:"clusterIdentifier,omitempty"`

	// ArgoProjectId is the ArgoCD AppProject name (random 8-char ID) that Harness
	// creates and maps to this agent's Harness project after cluster registration.
	// Used as the `project:` field in ApplicationSets and Applications.
	ArgoProjectId string `json:"argoProjectId,omitempty"`

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
