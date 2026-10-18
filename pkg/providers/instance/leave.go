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

package instance

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dklesev/karpenter-provider-ssh/internal/profile"
	"github.com/dklesev/karpenter-provider-ssh/internal/sshexec"
	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/metrics"
)

const (
	hostLeaveTimeout = 3 * time.Minute
	// The reboot script only schedules the reboot; core then waits for the
	// new boot itself.
	hostRebootTimeout = time.Minute
)

// SSHKeySecretKey and KnownHostSecretKey are the data keys read from the
// Secret referenced by SSHHost.spec.sshKeySecretRef.
const (
	SSHKeySecretKey    = "privateKey"
	KnownHostSecretKey = "knownHost"
)

// SSHCredentials is the material read from the host's SSH key Secret.
type SSHCredentials struct {
	PrivateKey []byte
	// PinnedHostKey is the pre-pinned fingerprint from the Secret's optional
	// knownHost key ("" when absent).
	PinnedHostKey string
}

// EffectivePin resolves the host key pin: an operator-provided knownHost from
// the Secret is authoritative (it survives key rotation); otherwise the
// TOFU-pinned fingerprint from the host status; otherwise "" (TOFU).
func (c *SSHCredentials) EffectivePin(h *v1beta1.SSHHost) string {
	if c.PinnedHostKey != "" {
		return c.PinnedHostKey
	}
	return h.Status.HostKeyFingerprint
}

// SSHTarget builds the SSH endpoint for a host with its credentials — the one
// place the SSHHost spec is mapped onto a connection.
func SSHTarget(h *v1beta1.SSHHost, creds *SSHCredentials) sshexec.Target {
	return sshexec.Target{
		Address:       h.Spec.Address,
		Port:          h.Spec.Port,
		User:          h.Spec.User,
		PrivateKey:    creds.PrivateKey,
		PinnedHostKey: creds.EffectivePin(h),
	}
}

// LeaveHost runs the leave phase of the host's installed profile over SSH
// via the given executor. Shared by the release path (Delete) and the
// hostprobe zombie guard, which disconnects hosts that rejoined on their own
// (e.g. after a reboot with a leave that did not disable kubelet).
func LeaveHost(ctx context.Context, kubeClient client.Client, kubernetesInterface kubernetes.Interface, exec sshexec.Runner, execShim sshexec.ShimRunner, h *v1beta1.SSHHost) error {
	return runInstalledPhase(ctx, kubeClient, kubernetesInterface, exec, execShim, h, profile.PhaseLeave, hostLeaveTimeout)
}

// runInstalledPhase runs a phase of the host's installed profile — leave or
// reboot, the phases that run with the host alone. Secrets are read through
// the plain clientset: the cached client would demand a cluster-wide Secret
// watch the scoped RBAC deliberately does not grant. Failures caused by the
// profile rather than the host are configErrors.
func runInstalledPhase(ctx context.Context, kubeClient client.Client, kubernetesInterface kubernetes.Interface, exec sshexec.Runner, execShim sshexec.ShimRunner, h *v1beta1.SSHHost, phase string, def time.Duration) error {
	if h.Status.InstalledProfile == "" {
		return &configError{fmt.Errorf("host %s has no installed profile to %s with", h.Name, phase)}
	}
	profName := strings.SplitN(h.Status.InstalledProfile, "@", 2)[0]
	prof := &v1beta1.SSHJoinProfile{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: profName}, prof); err != nil {
		return fmt.Errorf("getting profile %s: %w", profName, err)
	}
	if profile.Script(prof, phase) == "" {
		return &configError{fmt.Errorf("profile %s has no %s script", profName, phase)}
	}

	creds, err := ReadSSHCredentials(ctx, kubernetesInterface, h)
	if err != nil {
		return err
	}

	// No node class in hand here (the zombie guard has no NodeClaim at all), so
	// the script renders against a bare context — profiles whose leave or
	// reboot depends on vars or secrets are rejected by profile.Validate before
	// any NodeClaim can reach this path.
	pctx := &profile.Context{HostAddress: h.Spec.Address, NodeAddress: h.Spec.NodeIP()}
	env, err := pctx.Env()
	if err != nil {
		return err
	}
	target := SSHTarget(h, creds)

	phaseCtx, cancel := context.WithTimeout(ctx, profile.Timeout(prof, phase, def))
	defer cancel()
	start := time.Now()
	if h.Spec.ExecMode == v1beta1.ExecModeVerified {
		err = runVerifiedPhase(phaseCtx, execShim, target, h, prof, phase, env)
	} else {
		var script string
		if script, err = profile.Render(prof, phase, pctx); err != nil {
			err = &configError{err}
		} else {
			_, err = exec(phaseCtx, target, script, env)
		}
	}
	metrics.ObservePhase(phase, start, err)
	if err != nil {
		return fmt.Errorf("%s phase: %w", phase, err)
	}
	return nil
}

// ReadSSHCredentials fetches the host's SSH private key and optional
// pre-pinned host key via the plain clientset (uncached — see LeaveHost).
func ReadSSHCredentials(ctx context.Context, kubernetesInterface kubernetes.Interface, h *v1beta1.SSHHost) (*SSHCredentials, error) {
	secret, err := kubernetesInterface.CoreV1().Secrets(h.Namespace).Get(ctx, h.Spec.SSHKeySecretRef.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting ssh key secret: %w", err)
	}
	key, ok := secret.Data[SSHKeySecretKey]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s has no %q", h.Namespace, h.Spec.SSHKeySecretRef.Name, SSHKeySecretKey)
	}
	creds := &SSHCredentials{PrivateKey: key}
	if kh, ok := secret.Data[KnownHostSecretKey]; ok {
		fp, err := sshexec.PinnedFingerprintFromKnownHost(kh)
		if err != nil {
			return nil, fmt.Errorf("secret %s/%s %q: %w", h.Namespace, h.Spec.SSHKeySecretRef.Name, KnownHostSecretKey, err)
		}
		creds.PinnedHostKey = fp
	}
	return creds, nil
}
