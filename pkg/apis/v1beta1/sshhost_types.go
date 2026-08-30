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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// HostState describes the lifecycle state of an SSHHost.
type HostState string

const (
	// HostStatePending: registered but not yet probed healthy.
	HostStatePending HostState = "Pending"
	// HostStateAvailable: probed healthy and claimable.
	HostStateAvailable HostState = "Available"
	// HostStateClaimed: reserved for a NodeClaim; install/join in progress.
	HostStateClaimed HostState = "Claimed"
	// HostStateInUse: joined and backing a registered node.
	HostStateInUse HostState = "InUse"
	// HostStateUnhealthy: last probe failed; retried on the probe interval.
	HostStateUnhealthy HostState = "Unhealthy"
	// HostStateLeaving fences the zombie guard's leave: while set, the host is
	// not claimable, closing the race between a fresh claim and a leave in
	// flight.
	HostStateLeaving HostState = "Leaving"
	// HostStateMaintenance: operator-parked via the maintenance annotation;
	// never claimable.
	HostStateMaintenance HostState = "Maintenance"
)

const (
	// MaintenanceAnnotation, when present on an SSHHost, moves it to Maintenance.
	MaintenanceAnnotation = "karpenter.dklesev.github.io/maintenance"
	// HostClassLabel groups hosts into classes; classes become instance types.
	HostClassLabel = "karpenter.dklesev.github.io/host-class"
	// DefaultHostClass is the class of hosts carrying no HostClassLabel.
	DefaultHostClass = "default"
	// ProviderPrefix is the providerID scheme.
	ProviderPrefix = "kpssh://"
)

// HostClass returns the host's class. Unlabeled hosts fall into
// DefaultHostClass — every consumer (instance types, claim matching, metrics)
// must agree on that, or a class is advertised to karpenter that no host can
// ever satisfy, and every launch against it fails with insufficient capacity.
func HostClass(h *SSHHost) string {
	if c := h.Labels[HostClassLabel]; c != "" {
		return c
	}
	return DefaultHostClass
}

// ExecMode selects how the controller runs profile scripts on a host.
type ExecMode string

const (
	// ExecModeRaw streams the rendered script to `sudo bash -s` (default).
	ExecModeRaw ExecMode = "Raw"
	// ExecModeVerified streams a signed script to kpssh-shim (pinned via sshd
	// ForceCommand); the shim verifies the signature against a trusted signer
	// before executing. Requires the host provisioned for it (shim +
	// allowed_signers + ForceCommand) and the profile carrying signatures.
	ExecModeVerified ExecMode = "Verified"
)

// SSHHostSpec defines a pre-existing host reachable over SSH.
//
// The execMode rule closes a silent posture downgrade: Verified with no
// trustedSigners would skip the controller-side signature check without any
// visible signal, leaving only the host shim as a gate.
// +kubebuilder:validation:XValidation:rule="!has(self.execMode) || self.execMode != 'Verified' || (has(self.trustedSigners) && size(self.trustedSigners) > 0)",message="execMode Verified requires at least one trustedSigners entry"
type SSHHostSpec struct {
	// Address is the IP or DNS name the provider connects to. Immutable: it is
	// the host's only identity, and repointing a claimed host would run its
	// leave against a different machine while the joined one keeps running.
	// Re-IP by deleting and recreating the SSHHost.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="address is immutable — delete and recreate the SSHHost to re-address a host"
	Address string `json:"address"`

	// Port is the SSH port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=22
	// +optional
	Port int32 `json:"port,omitempty"`

	// User must be able to run the profile scripts via sudo.
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:default=root
	// +optional
	User string `json:"user,omitempty"`

	// SSHKeySecretRef references a Secret (same namespace) with keys
	// "privateKey" (required, PEM) and optional "knownHost".
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="sshKeySecretRef.name must not be empty"
	SSHKeySecretRef corev1.LocalObjectReference `json:"sshKeySecretRef"`

	// NodeAddress is the IP the node advertises in-cluster (kubelet --node-ip).
	// Defaults to Address; set when SSH goes through a different path. Must be
	// an IP literal — the kubelet's --node-ip does not accept names, and the
	// zombie guard matches it against Node InternalIPs.
	// isIP rather than a character-class pattern: `^[0-9a-fA-F:.]+$` admits
	// "abc", "cafe.beef", "999.999.999.999" and ":::::" — every one of which
	// passes admission and then surfaces as a kubelet that will not start, or a
	// zombie guard that silently never matches a Node. Needs k8s >= 1.31 for the
	// CEL IP library; the chart's kubeVersion floor is set accordingly.
	// +kubebuilder:validation:MaxLength=45
	// +kubebuilder:validation:XValidation:rule="isIP(self)",message="nodeAddress must be an IP literal (kubelet --node-ip does not accept names)"
	// +optional
	NodeAddress string `json:"nodeAddress,omitempty"`

	// Capacity the host provides when joined (cpu, memory, optionally
	// nvidia.com/gpu etc.). Should be uniform within a host class: a class
	// advertises the per-resource minimum across its hosts, so an undersized
	// member shrinks the whole class.
	// +kubebuilder:validation:XValidation:rule="'cpu' in self && 'memory' in self",message="capacity must declare at least cpu and memory"
	Capacity corev1.ResourceList `json:"capacity"`

	// ExecMode selects script execution on this host: Raw (`sudo bash -s`) or
	// Verified (signed scripts via kpssh-shim, pinned by sshd ForceCommand).
	// Set Verified only after the host is provisioned for it. Default Raw.
	// +kubebuilder:validation:Enum=Raw;Verified
	// +kubebuilder:default=Raw
	// +optional
	ExecMode ExecMode `json:"execMode,omitempty"`

	// TrustedSigners are OpenSSH public-key lines (allowed_signers key column)
	// the controller verifies profile signatures against before connecting —
	// defense in depth and fail-fast; the host shim re-verifies independently.
	// Required (non-empty) when execMode is Verified.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	TrustedSigners []string `json:"trustedSigners,omitempty"`

	// ShimCommand overrides the verified-exec shim path invoked over SSH
	// (default /opt/kpssh/kpssh-shim). A ForceCommand-locked host ignores it.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	ShimCommand string `json:"shimCommand,omitempty"`
}

// NodeIP returns the address the node advertises in-cluster.
func (s *SSHHostSpec) NodeIP() string {
	if s.NodeAddress != "" {
		return s.NodeAddress
	}
	return s.Address
}

// ClaimReference identifies the NodeClaim holding a host. Name is the claim
// lock's key; UID fences NodeClaim name reuse, so a Delete arriving late for a
// dead claim cannot tear down the node of its successor on the same host.
type ClaimReference struct {
	// Name of the NodeClaim (cluster-scoped).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// UID of the NodeClaim at claim time.
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

// NodeInternalIPIndex is the field-index key for looking a corev1.Node up by
// its InternalIP. Registered on the manager in pkg/operator; the alternative —
// listing every Node and scanning addresses in Go — deep-copies the whole
// cluster's Nodes on a path that polls every few seconds per joining host.
const NodeInternalIPIndex = "status.addresses.internalIP"

// NodeInternalIPs is the body of that index: a Node's InternalIP addresses.
// It lives here, next to the key, so the operator (which registers the index)
// and tests (which must mirror it onto their fake clients) cannot drift —
// an unregistered or mismatched field selector fails at RUNTIME, not at
// compile time.
func NodeInternalIPs(node *corev1.Node) []string {
	var ips []string
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			ips = append(ips, a.Address)
		}
	}
	return ips
}

// HostConditionReady is the condition type reporting whether the probe last
// reached the host and found it usable. It is an observation for humans and
// `kubectl wait`; State remains the field the claim lock turns on.
const HostConditionReady = "Ready"

// Reasons for HostConditionReady.
const (
	HostReadyReasonProbeSucceeded = "ProbeSucceeded"
	HostReadyReasonProbeFailed    = "ProbeFailed"
	HostReadyReasonPending        = "AwaitingProbe"
	HostReadyReasonMaintenance    = "Maintenance"
)

// SSHHostStatus is the observed state of the host.
type SSHHostStatus struct {
	// State is the host's lifecycle state (see HostState). This is the field
	// the claim lock compare-and-swaps on; treat it as authoritative, and the
	// Ready condition below as the human-readable observation of the same
	// probe. (Conditions make a poor distributed lock, so State stays.)
	// +kubebuilder:validation:Enum=Pending;Available;Claimed;InUse;Unhealthy;Leaving;Maintenance
	// +optional
	State HostState `json:"state,omitempty"`

	// Conditions holds the host's observations. "Ready" is true while the probe
	// can reach the host and it is not parked Unhealthy or in Maintenance,
	// which makes `kubectl wait --for=condition=Ready sshhost/<name>` work and
	// matches every other kind in the karpenter ecosystem.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ClaimRef points to the NodeClaim currently holding this host.
	// Setting it is the claim lock (optimistic concurrency).
	// +optional
	ClaimRef *ClaimReference `json:"claimRef,omitempty"`

	// InstalledProfile records "<profile>@<version>" after a successful install.
	// +optional
	InstalledProfile string `json:"installedProfile,omitempty"`

	// BootstrapTokenID is the id of the kubelet TLS-bootstrap token minted for
	// the in-flight join. The probe controller deletes the token's Secret once
	// the node has registered (the kubelet holds a real client certificate by
	// then) and clears this field; the release path deletes it if the join
	// never got that far. Without this, expired token Secrets accumulate in
	// kube-system forever — kube-controller-manager's tokencleaner is disabled
	// by default upstream.
	// +kubebuilder:validation:MaxLength=6
	// +optional
	BootstrapTokenID string `json:"bootstrapTokenID,omitempty"`

	// ProviderID is the providerID of the node this host currently backs —
	// kpssh://… in static mode, or the adopted external form (e.g.
	// eks-hybrid:///…). Lookup key for Delete/Get.
	// +optional
	ProviderID string `json:"providerID,omitempty"`

	// HostKeyFingerprint is TOFU-pinned on first contact unless pre-seeded.
	// +optional
	HostKeyFingerprint string `json:"hostKeyFingerprint,omitempty"`

	// ObservedCapacity as probed (nproc, MemTotal). When it falls short of
	// spec.capacity, the probe controller emits a CapacityDrift warning event.
	// +optional
	ObservedCapacity corev1.ResourceList `json:"observedCapacity,omitempty"`

	// ObservedArch as probed (uname -m normalized: amd64/arm64).
	// +optional
	ObservedArch string `json:"observedArch,omitempty"`

	// ObservedGeneration is the spec generation last acted on by the probe;
	// a spec change bypasses the probe-interval backoff.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastProbeTime is when the probe last ran against this host.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`

	// LastProbeError is the last probe failure ("" once healthy again).
	// +optional
	LastProbeError string `json:"lastProbeError,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sshh,categories=karpenter
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.address`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.metadata.labels['karpenter\.dklesev\.github\.io/host-class']`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=`.status.claimRef.name`
// +kubebuilder:printcolumn:name="Installed",type=string,JSONPath=`.status.installedProfile`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SSHHost is a pool inventory entry: a pre-existing host joinable as a node.
type SSHHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SSHHostSpec   `json:"spec,omitempty"`
	Status SSHHostStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SSHHostList contains a list of SSHHost.
type SSHHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SSHHost `json:"items"`
}
