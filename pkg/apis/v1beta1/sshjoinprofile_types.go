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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProfileScripts holds the four lifecycle phase scripts. Each is a Go
// text/template rendered with the join context and executed on the host via
// `sudo bash` with KPSSH_* env injected. All scripts MUST be idempotent.
type ProfileScripts struct {
	// Install prepares a blank host (heavy, once per host, cached via
	// SSHHost.status.installedProfile).
	// +kubebuilder:validation:MaxLength=262144
	Install string `json:"install"`

	// Join connects the host to the cluster (fast, every claim).
	// +kubebuilder:validation:MaxLength=262144
	Join string `json:"join"`

	// Leave disconnects the host, keeps installed components (fast, every
	// release). Rendered with an empty context — the release path and the
	// zombie guard both run it without a node class in hand, so it must not
	// reference .Vars or .Secrets (the nodeclass controller rejects profiles
	// that do).
	// +kubebuilder:validation:MaxLength=262144
	Leave string `json:"leave"`

	// Uninstall fully cleans the host. Manual/repair only.
	// +kubebuilder:validation:MaxLength=262144
	// +optional
	Uninstall string `json:"uninstall,omitempty"`
}

// ProfileTimeouts bounds script execution per phase. Slow bare-metal installs
// can legitimately exceed the defaults; note install+join must finish inside
// karpenter core's 15-minute registration TTL or the NodeClaim is terminated.
//
// The patterns are load-bearing: metav1.Duration only decodes Go duration
// strings, and a single object carrying an undecodable one would fail the
// typed List of every informer watching this kind — wedging the controller
// cluster-wide, not just for that profile.
type ProfileTimeouts struct {
	// Install bounds the install script (default 10m).
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(s|m|h))+$`
	// +optional
	Install *metav1.Duration `json:"install,omitempty"`

	// Join bounds the join script (default 5m).
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(s|m|h))+$`
	// +optional
	Join *metav1.Duration `json:"join,omitempty"`

	// Leave bounds the leave script (default 3m).
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(s|m|h))+$`
	// +optional
	Leave *metav1.Duration `json:"leave,omitempty"`
}

// ProfileSignatures holds the armored SSHSIG for each phase script, required by
// hosts in Verified exec mode. Produce them offline in CI:
// `ssh-keygen -Y sign -n kpssh <script>`. Both the controller (before it
// connects) and the host shim (before it runs) verify the script against these
// signatures using a trusted signer — a compromised controller cannot forge one.
type ProfileSignatures struct {
	// Install is the SSHSIG over spec.scripts.install.
	// +kubebuilder:validation:MaxLength=16384
	// +optional
	Install string `json:"install,omitempty"`

	// Join is the SSHSIG over spec.scripts.join.
	// +kubebuilder:validation:MaxLength=16384
	// +optional
	Join string `json:"join,omitempty"`

	// Leave is the SSHSIG over spec.scripts.leave.
	// +kubebuilder:validation:MaxLength=16384
	// +optional
	Leave string `json:"leave,omitempty"`

	// Uninstall is the SSHSIG over spec.scripts.uninstall.
	// +kubebuilder:validation:MaxLength=16384
	// +optional
	Uninstall string `json:"uninstall,omitempty"`
}

// SSHJoinProfileSpec defines a pluggable join mechanism.
type SSHJoinProfileSpec struct {
	// Version invalidates the install cache when bumped ("<name>@<version>").
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]{1,63}$`
	// +kubebuilder:default="1"
	// +optional
	Version string `json:"version,omitempty"`

	Scripts ProfileScripts `json:"scripts"`

	// Timeouts overrides the per-phase script deadlines.
	// +optional
	Timeouts *ProfileTimeouts `json:"timeouts,omitempty"`

	// Signatures are the per-phase SSHSIG signatures over the scripts, required
	// when a Verified-mode host runs a phase. Scripts must be template-free in
	// that case (the signed bytes must equal the executed bytes); all per-node
	// variability arrives as KPSSH_* params instead.
	// +optional
	Signatures *ProfileSignatures `json:"signatures,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=sjp,categories=karpenter
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SSHJoinProfile is a named, versioned set of install/join/leave/uninstall scripts.
type SSHJoinProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SSHJoinProfileSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// SSHJoinProfileList contains a list of SSHJoinProfile.
type SSHJoinProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SSHJoinProfile `json:"items"`
}
