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

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// HarnessGitopsAgentSpec defines the desired state of HarnessGitopsAgent
// HarnessGitopsAgentSpec defines the desired state of HarnessGitopsAgent
type HarnessGitopsAgentSpec struct {
	// Name is the name of the Harness GitOps Agent
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// AccountId is the Harness Account Identifier
	// +kubebuilder:validation:Required
	AccountId string `json:"accountId"`

	// OrgId is the Harness Organization Identifier
	// +kubebuilder:validation:Required
	OrgId string `json:"orgId"`

	// ProjectId is the Harness Project Identifier
	// +kubebuilder:validation:Required
	ProjectId string `json:"projectId"`

	// Type of agent (e.g., "KUBERNETES")
	// +kubebuilder:default:="KUBERNETES"
	Type string `json:"type,omitempty"`

	// ApiKeySecretRef is the name of the Secret containing the Harness API Key
	// Key inside secret must be "api_key"
	// +kubebuilder:validation:Required
	ApiKeySecretRef string `json:"apiKeySecretRef"`

	// TokenSecretRef is the name of the Secret where the generated Agent Token will be stored
	// +kubebuilder:validation:Required
	TokenSecretRef string `json:"tokenSecretRef"`
}

// HarnessGitopsAgentStatus defines the observed state of HarnessGitopsAgent.
// HarnessGitopsAgentStatus defines the observed state of HarnessGitopsAgent
type HarnessGitopsAgentStatus struct {
	// AgentIdentifier is the ID returned by Harness after registration
	AgentIdentifier string `json:"agentIdentifier,omitempty"`

	// Conditions store the detailed state transitions
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

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
