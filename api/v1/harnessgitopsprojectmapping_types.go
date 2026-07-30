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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// HarnessGitopsAgentReference identifies an Agent in the Mapping namespace.
type HarnessGitopsAgentReference struct {
	// Name is the name of the HarnessGitopsAgent resource.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// HarnessGitopsProjectMappingSpec defines one Argo CD AppProject to Harness
// project mapping. Mapping identity is immutable; replace the resource to
// target a different Agent, AppProject, or Harness project.
// +kubebuilder:validation:XValidation:rule="self.agentRef.name == oldSelf.agentRef.name && self.appProject == oldSelf.appProject && (has(self.orgId) ? self.orgId : \"\") == (has(oldSelf.orgId) ? oldSelf.orgId : \"\") && (has(self.projectId) ? self.projectId : \"\") == (has(oldSelf.projectId) ? oldSelf.projectId : \"\") && (has(self.autoCreateServiceEnv) ? self.autoCreateServiceEnv : false) == (has(oldSelf.autoCreateServiceEnv) ? oldSelf.autoCreateServiceEnv : false)",message="Harness project mapping identity is immutable; replace the resource instead"
type HarnessGitopsProjectMappingSpec struct {
	// AgentRef selects a HarnessGitopsAgent in the same namespace.
	// +kubebuilder:validation:Required
	AgentRef HarnessGitopsAgentReference `json:"agentRef"`

	// AppProject is the Argo CD AppProject name to map.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	AppProject string `json:"appProject"`

	// OrgID is required for ACCOUNT-scoped Agents and inherited for narrower scopes.
	// Empty and omitted are equivalent.
	// +optional
	OrgID string `json:"orgId,omitempty"`

	// ProjectID is required for ACCOUNT- and ORG-scoped Agents and inherited for
	// PROJECT-scoped Agents. Empty and omitted are equivalent.
	// +optional
	ProjectID string `json:"projectId,omitempty"`

	// AutoCreateServiceEnv allows Harness to create matching service and
	// environment resources for this mapping.
	// +kubebuilder:default:=false
	// +optional
	AutoCreateServiceEnv bool `json:"autoCreateServiceEnv,omitempty"`

	// AdoptMappingID requests ownership of an exact existing Harness mapping. It
	// also recovers a create whose outcome was ambiguous. It is correctable
	// recovery input, not mapping identity. The controller must verify the ID and
	// complete remote tuple before accepting it as Adopted.
	// +optional
	AdoptMappingID string `json:"adoptMappingId,omitempty"`
}

// HarnessGitopsProjectMappingAgentStatus is the resolved Harness Agent lookup
// tuple used by mapping API calls.
type HarnessGitopsProjectMappingAgentStatus struct {
	// Identifier is the resolved Harness GitOps Agent identifier.
	// +kubebuilder:validation:MinLength=1
	Identifier string `json:"identifier"`

	// AccountID is the resolved Harness account identifier.
	// +kubebuilder:validation:MinLength=1
	AccountID string `json:"accountId"`

	// Scope is the Harness Agent scope.
	// +kubebuilder:validation:Enum=ACCOUNT;ORG;PROJECT
	Scope string `json:"scope"`

	// OrgID is the Agent organization identifier. It is empty at ACCOUNT scope.
	OrgID string `json:"orgId"`

	// ProjectID is the Agent project identifier. It is empty at ACCOUNT and ORG scope.
	ProjectID string `json:"projectId"`
}

// HarnessGitopsProjectMappingTargetStatus is the resolved Harness project
// mapping tuple.
type HarnessGitopsProjectMappingTargetStatus struct {
	// OrgID is the resolved target organization identifier.
	// +kubebuilder:validation:MinLength=1
	OrgID string `json:"orgId"`

	// ProjectID is the resolved target project identifier.
	// +kubebuilder:validation:MinLength=1
	ProjectID string `json:"projectId"`

	// AppProject is the observed Argo CD AppProject name.
	// +kubebuilder:validation:MinLength=1
	AppProject string `json:"appProject"`

	// AutoCreateServiceEnv is the option observed on the remote mapping.
	AutoCreateServiceEnv bool `json:"autoCreateServiceEnv"`
}

// HarnessGitopsProjectMappingRemoteStatus is the resolved remote tuple used to
// create, verify, and safely clean up a Harness mapping. Ownership remains
// empty until provenance is verified. MappingID may be set while Pending after
// a confirmed create response, or while OutcomeUnknown as a diagnostic
// candidate that still requires explicit adoption.
// +kubebuilder:validation:XValidation:rule="!has(self.ownership) || (has(self.mappingId) && self.mappingId.size() > 0)",message="mapping ownership requires a remote mapping ID"
type HarnessGitopsProjectMappingRemoteStatus struct {
	// MappingID is the identifier returned by Harness.
	// +optional
	MappingID string `json:"mappingId,omitempty"`

	// Ownership determines whether finalization may delete the remote mapping.
	// +optional
	Ownership ResourceOwnership `json:"ownership,omitempty"`

	// Agent is the exact Agent lookup tuple used for the verified mapping.
	Agent HarnessGitopsProjectMappingAgentStatus `json:"agent"`

	// Target is the exact project mapping tuple observed in Harness.
	Target HarnessGitopsProjectMappingTargetStatus `json:"target"`
}

// MappingCreationState records an in-flight or ambiguous remote create.
// +kubebuilder:validation:Enum=Pending;OutcomeUnknown
type MappingCreationState string

const (
	// MappingCreationPending means create intent was persisted. MappingID remains
	// empty before the API call and is populated after a confirmed response while
	// exact-ID verification is still pending.
	MappingCreationPending MappingCreationState = "Pending"
	// MappingCreationOutcomeUnknown means the API call may have committed remotely.
	MappingCreationOutcomeUnknown MappingCreationState = "OutcomeUnknown"
)

// HarnessGitopsProjectMappingStatus defines the observed state of one mapping.
// +kubebuilder:validation:XValidation:rule="!has(self.creationState) || has(self.remote)",message="mapping creation state requires the resolved remote tuple"
type HarnessGitopsProjectMappingStatus struct {
	// ObservedGeneration is the most recent generation processed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CreationState distinguishes a never-attempted create from an operation
	// that may need explicit ownership recovery.
	// +optional
	CreationState MappingCreationState `json:"creationState,omitempty"`

	// Remote contains the resolved Harness tuple and any observed mapping ID.
	// +optional
	Remote *HarnessGitopsProjectMappingRemoteStatus `json:"remote,omitempty"`

	// Conditions store mapping readiness and lifecycle state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hgapm
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="AppProject",type=string,JSONPath=`.spec.appProject`
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.status.remote.target.projectId`
// +kubebuilder:printcolumn:name="Ownership",type=string,JSONPath=`.status.remote.ownership`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// HarnessGitopsProjectMapping is one AppProject to Harness project mapping.
type HarnessGitopsProjectMapping struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired mapping.
	// +required
	Spec HarnessGitopsProjectMappingSpec `json:"spec"`

	// status defines the observed mapping.
	// +optional
	Status HarnessGitopsProjectMappingStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// HarnessGitopsProjectMappingList contains HarnessGitopsProjectMapping resources.
type HarnessGitopsProjectMappingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []HarnessGitopsProjectMapping `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HarnessGitopsProjectMapping{}, &HarnessGitopsProjectMappingList{})
}
