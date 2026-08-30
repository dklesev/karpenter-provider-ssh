// Copyright The karpenter-provider-ssh Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1beta1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SSHNodeClassKind is the Kind string carried in a NodePool/NodeClaim
// nodeClassRef pointing at this provider. Used to skip NodePools owned by a
// coexisting cloudprovider (e.g. AWS karpenter's EC2NodeClass).
const SSHNodeClassKind = "SSHNodeClass"

// ProviderIDSource selects who decides the node's providerID.
type ProviderIDSource string

const (
	// ProviderIDSourceStatic: the provider decides (kpssh://…), passed to the
	// kubelet via --provider-id.
	ProviderIDSourceStatic ProviderIDSource = "Static"
	// ProviderIDSourceAdopt: the join mechanism owns node identity (e.g. EKS
	// nodeadm); the provider adopts the providerID from the registered Node.
	ProviderIDSourceAdopt ProviderIDSource = "Adopt"
)

// SSHNodeClassSpec configures how NodeClaims of this class turn pool hosts
// into nodes.
type SSHNodeClassSpec struct {
	// HostSelector limits which SSHHosts may be claimed. Empty = all hosts
	// in the provider namespace.
	// +optional
	HostSelector *metav1.LabelSelector `json:"hostSelector,omitempty"`

	// JoinProfileRef names the SSHJoinProfile used for hosts of this class.
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="joinProfileRef.name must not be empty"
	JoinProfileRef corev1.LocalObjectReference `json:"joinProfileRef"`

	// Vars are profile template variables (e.g. k8sMinor, clusterDNS). Keys
	// become KPSSH_VAR_<key> environment variables in the scripts and must be
	// valid shell identifiers.
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[A-Za-z_][A-Za-z0-9_]*$'))",message="vars keys must be valid shell identifiers ([A-Za-z_][A-Za-z0-9_]*)"
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(self[k]) <= 2048)",message="vars values must be at most 2048 characters"
	// +optional
	Vars map[string]string `json:"vars,omitempty"`

	// JoinSecretRef names a Secret in the pool namespace whose data keys are
	// injected into join scripts as KPSSH_SECRET_<UPPERCASED_KEY> env vars.
	// Generic credential material passing (e.g. EKS SSM activation id/code);
	// the provider only reads the Secret — its lifecycle is out of scope.
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="joinSecretRef.name must not be empty"
	// +optional
	JoinSecretRef *corev1.LocalObjectReference `json:"joinSecretRef,omitempty"`

	// ProviderIDSource: Static (provider decides kpssh://…, passed via
	// --provider-id) or Adopt (join mechanism owns node identity, e.g. EKS
	// nodeadm; provider copies the Node's providerID).
	// +kubebuilder:validation:Enum=Static;Adopt
	// +kubebuilder:default=Static
	// +optional
	ProviderIDSource ProviderIDSource `json:"providerIDSource,omitempty"`

	// PricePerCPUHour models the cost of an attached vCPU-hour (EKS hybrid:
	// 0.02). Drives karpenter's cost-aware decisions. Unit: USD, string form.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +kubebuilder:default="0.02"
	// +optional
	PricePerCPUHour string `json:"pricePerCPUHour,omitempty"`

	// KubeReserved overrides the kubelet overhead modeled per instance type
	// (default: 80m CPU, 300Mi memory). Advertised to karpenter only — align
	// the kubelet's --kube-reserved via the join profile.
	// +kubebuilder:validation:XValidation:rule="self.all(k, k in ['cpu','memory','ephemeral-storage'])",message="kubeReserved keys must be cpu, memory or ephemeral-storage"
	// +optional
	KubeReserved corev1.ResourceList `json:"kubeReserved,omitempty"`

	// MaxPods overrides the pods capacity advertised per instance type
	// (default: 110). Advertised to karpenter only — align the kubelet's
	// maxPods via the join profile.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	MaxPods *int32 `json:"maxPods,omitempty"`

	// Cluster overrides endpoint/CA discovery. When empty, both are read
	// from the kube-public/cluster-info ConfigMap (standard TLS-bootstrap
	// discovery), which is correct when the provider runs in the cluster it
	// joins nodes to.
	// +optional
	Cluster *ClusterAccess `json:"cluster,omitempty"`
}

// ClusterAccess pins the API endpoint and CA handed to joining kubelets.
type ClusterAccess struct {
	// Endpoint is the API server URL, e.g. https://10.0.0.5:6443.
	// +kubebuilder:validation:Pattern=`^https://.+$`
	// +kubebuilder:validation:MaxLength=253
	Endpoint string `json:"endpoint"`

	// CABundle is the cluster CA certificate (PEM), base64-encoded on the wire
	// like every other Kubernetes byte field (webhook clientConfig, APIService).
	// +kubebuilder:validation:MaxLength=65536
	// +optional
	CABundle []byte `json:"caBundle,omitempty"`
}

// SSHNodeClassStatus reports readiness.
type SSHNodeClassStatus struct {
	// Conditions carries the Ready condition set by the nodeclass controller
	// (profile exists and parses, selector matches at least one host).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=snc,categories=karpenter
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.joinProfileRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SSHNodeClass is the karpenter node class for pool-backed nodes.
type SSHNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SSHNodeClassSpec   `json:"spec,omitempty"`
	Status SSHNodeClassStatus `json:"status,omitempty"`
}

const (
	// TerminationFinalizer blocks SSHNodeClass deletion while NodeClaims still
	// reference it — deleting the class first would strand their Delete path.
	TerminationFinalizer = "karpenter.dklesev.github.io/termination"

	// AnnotationNodeClassHash is stamped onto NodeClaims at Create with the
	// nodeclass's JoinHash; IsDrifted compares it against the current spec so
	// join-affecting nodeclass edits roll existing nodes.
	AnnotationNodeClassHash = "karpenter.dklesev.github.io/nodeclass-hash"
	// AnnotationNodeClassHashVersion versions the hash algorithm; on mismatch
	// drift detection abstains instead of rolling the fleet on a provider
	// upgrade.
	AnnotationNodeClassHashVersion = "karpenter.dklesev.github.io/nodeclass-hash-version"
	// NodeClassHashVersion is the current JoinHash algorithm version.
	NodeClassHashVersion = "v1"
)

// JoinHash digests the spec fields that feed the rendered join (vars, join
// secret ref, providerID mode, cluster override). Scheduling-model-only
// fields (selector, pricing, kubeReserved, maxPods) are deliberately
// excluded — changing them must not roll nodes. The profile itself is
// covered separately by the installed-marker (ProfileDrift).
func (n *SSHNodeClass) JoinHash() string {
	b, _ := json.Marshal(struct {
		Vars             map[string]string            `json:"vars,omitempty"`
		JoinSecretRef    *corev1.LocalObjectReference `json:"joinSecretRef,omitempty"`
		ProviderIDSource ProviderIDSource             `json:"providerIDSource,omitempty"`
		Cluster          *ClusterAccess               `json:"cluster,omitempty"`
	}{n.Spec.Vars, n.Spec.JoinSecretRef, n.Spec.ProviderIDSource, n.Spec.Cluster})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// StatusConditions implements the operatorpkg status.Object interface.
func (n *SSHNodeClass) StatusConditions(_ ...status.ForOption) status.ConditionSet {
	return status.NewReadyConditions().For(n)
}

// GetConditions implements status.Object.
func (n *SSHNodeClass) GetConditions() []status.Condition { return n.Status.Conditions }

// SetConditions implements status.Object.
func (n *SSHNodeClass) SetConditions(conditions []status.Condition) {
	n.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// SSHNodeClassList contains a list of SSHNodeClass.
type SSHNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SSHNodeClass `json:"items"`
}
