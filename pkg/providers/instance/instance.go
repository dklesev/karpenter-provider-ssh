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

// Package instance turns NodeClaims into joined pool hosts and back:
// Create = claim + install (cached) + join over SSH; Delete = leave + release.
package instance

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/dklesev/karpenter-provider-ssh/internal/profile"
	"github.com/dklesev/karpenter-provider-ssh/internal/sshexec"
	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/metrics"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/bootstrap"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/host"
)

const (
	installTimeout    = 10 * time.Minute
	joinTimeout       = 5 * time.Minute
	tokenTTL          = 1 * time.Hour
	adoptTimeout      = 4 * time.Minute
	adoptPollInterval = 5 * time.Second
)

// Instance describes a joined (or joining) host for the cloudprovider layer.
type Instance struct {
	ProviderID   string
	HostName     string
	NodeName     string
	Class        string
	Capacity     corev1.ResourceList
	CreationTime time.Time
	// InstalledProfile is the host's install-cache marker ("<profile>@<version>");
	// drift detection compares it against the node class's current profile.
	InstalledProfile string
}

// Provider is the instance lifecycle interface.
type Provider interface {
	Create(ctx context.Context, nodeClass *v1beta1.SSHNodeClass, nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) (*Instance, error)
	// Delete takes the whole NodeClaim, not just its providerID: static
	// providerIDs are host-scoped, so the claim's identity is what tells a
	// legitimate teardown apart from a stale Delete aimed at a host that has
	// since been re-claimed (see host.ClaimHeldBy).
	Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error
	Get(ctx context.Context, providerID string) (*Instance, error)
	List(ctx context.Context) ([]*Instance, error)
}

// DefaultProvider implements Provider. Secrets go through the plain clientset
// (kubernetesInterface) instead of the cached client — the cache would demand
// a cluster-wide Secret watch that the scoped RBAC deliberately forbids.
type DefaultProvider struct {
	kubeClient          client.Client
	kubernetesInterface kubernetes.Interface
	hostProvider        host.Provider
	bootstrapProvider   bootstrap.Provider
	exec                sshexec.Runner
	execShim            sshexec.ShimRunner
}

// NewDefaultProvider returns an instance provider.
func NewDefaultProvider(kubeClient client.Client, kubernetesInterface kubernetes.Interface, hostProvider host.Provider, bootstrapProvider bootstrap.Provider) *DefaultProvider {
	return &DefaultProvider{
		kubeClient:          kubeClient,
		kubernetesInterface: kubernetesInterface,
		hostProvider:        hostProvider,
		bootstrapProvider:   bootstrapProvider,
		exec:                sshexec.Run,
		execShim:            sshexec.RunShim,
	}
}

// WithExecutor swaps the SSH executor (tests).
func (p *DefaultProvider) WithExecutor(exec sshexec.Runner) *DefaultProvider {
	p.exec = exec
	return p
}

// configError marks a join failure caused by the profile or node class
// configuration — a missing signature, a template in a verified script, an
// unreadable secret — rather than by the host, which was never touched.
// Create releases such hosts back to Available: parking them Unhealthy would
// misreport one bad profile as a fleet-wide hardware fault, one host per
// launch attempt.
type configError struct{ err error }

func (e *configError) Error() string { return e.err.Error() }
func (e *configError) Unwrap() error { return e.err }

// isConfigError reports whether err (anywhere in its chain) is a configError.
func isConfigError(err error) bool {
	var ce *configError
	return errors.As(err, &ce)
}

// Create claims an Available host from the cheapest compatible instance type
// (host class) and joins it. Idempotent per NodeClaim: an existing claim is
// resumed, not duplicated.
func (p *DefaultProvider) Create(ctx context.Context, nodeClass *v1beta1.SSHNodeClass, nodeClaim *karpv1.NodeClaim, instanceTypes []*cloudprovider.InstanceType) (*Instance, error) {
	log := ctrllog.FromContext(ctx).WithValues("nodeClaim", nodeClaim.Name)

	// resume a half-done claim from a previous reconcile/restart
	h, err := p.hostProvider.ByClaim(ctx, nodeClaim)
	if err != nil {
		return nil, err
	}

	if h == nil {
		// cheapest class first (instanceTypes arrive filtered by karpenter's
		// requirements; order by offering price)
		for _, it := range sortByPrice(instanceTypes) {
			candidates, err := p.hostProvider.Available(ctx, nodeClass.Spec.HostSelector, it.Name)
			if err != nil {
				return nil, err
			}
			for _, c := range candidates {
				ok, err := p.hostProvider.Claim(ctx, c, nodeClaim)
				if err != nil {
					return nil, err
				}
				if ok {
					h = c
					break
				}
			}
			if h != nil {
				break
			}
		}
	}
	if h == nil {
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("no Available SSHHost for NodeClaim %s", nodeClaim.Name))
	}
	log.Info("host claimed", "host", h.Name, "class", v1beta1.HostClass(h))

	if err := p.join(ctx, nodeClass, nodeClaim, h); err != nil {
		// A config error indicts the profile/nodeclass, not the host: the host
		// was never touched and goes straight back to Available. Anything else
		// releases unhealthy — the probe loop re-admits the host once it checks
		// out. Either way karpenter retries the NodeClaim.
		p.release(ctx, h, isConfigError(err), fmt.Sprintf("join failed: %v", err))
		return nil, cloudprovider.NewCreateError(fmt.Errorf("joining host %s: %w", h.Name, err), "JoinFailed", "joining host over SSH failed")
	}

	providerID := host.ProviderID(h)
	if nodeClass.Spec.ProviderIDSource == v1beta1.ProviderIDSourceAdopt {
		// the join mechanism owns node identity (e.g. EKS nodeadm): wait for the
		// Node advertising this host's IP and adopt its providerID
		adopted, err := p.adoptProviderID(ctx, h)
		if err != nil {
			p.release(ctx, h, false, fmt.Sprintf("providerID adoption failed: %v", err))
			return nil, cloudprovider.NewCreateError(err, "AdoptFailed", "node did not appear for providerID adoption")
		}
		providerID = adopted
	}
	if err := p.hostProvider.SetProviderID(ctx, h, providerID); err != nil {
		return nil, err
	}
	if err := p.hostProvider.MarkInUse(ctx, h); err != nil {
		// non-fatal: the claim itself holds; the state label lags until the
		// next transition
		log.Error(err, "marking host in use", "host", h.Name)
	}

	return p.toInstance(h, nodeClaim.Name), nil
}

// release returns a host to the pool after a failed launch, deleting any
// bootstrap token minted for it first. healthy=true puts the host straight
// back to Available (the failure was not its fault); false parks it Unhealthy
// until the probe loop re-admits it. Failures of the release itself are
// logged, not masked: a lost optimistic-lock race means another actor already
// moved this host on, and it now owns its state.
func (p *DefaultProvider) release(ctx context.Context, h *v1beta1.SSHHost, healthy bool, reason string) {
	log := ctrllog.FromContext(ctx)
	p.deleteBootstrapToken(ctx, h)
	if err := p.hostProvider.Release(ctx, h, healthy, reason); err != nil {
		log.Error(err, "releasing host", "host", h.Name, "reason", reason)
	}
}

// deleteBootstrapToken removes the token minted for an in-flight join. Safe on
// the release path: the join either never completed, or the kubelet has long
// since traded the token for a client certificate.
func (p *DefaultProvider) deleteBootstrapToken(ctx context.Context, h *v1beta1.SSHHost) {
	if h.Status.BootstrapTokenID == "" {
		return
	}
	if err := p.bootstrapProvider.DeleteTokenByID(ctx, h.Status.BootstrapTokenID); err != nil {
		ctrllog.FromContext(ctx).Error(err, "deleting bootstrap token", "host", h.Name, "tokenID", h.Status.BootstrapTokenID)
	}
}

// adoptProviderID polls for a Node whose InternalIP matches the host and
// returns its providerID.
func (p *DefaultProvider) adoptProviderID(ctx context.Context, h *v1beta1.SSHHost) (string, error) {
	log := ctrllog.FromContext(ctx)
	var providerID string
	err := wait.PollUntilContextTimeout(ctx, adoptPollInterval, adoptTimeout, true, func(ctx context.Context) (bool, error) {
		nodes := &corev1.NodeList{}
		if err := p.kubeClient.List(ctx, nodes,
			client.MatchingFields{v1beta1.NodeInternalIPIndex: h.Spec.NodeIP()}); err != nil {
			// Transient: a failed List must not abort the wait and tear down a
			// node that joined perfectly well. The poll timeout bounds us.
			log.V(1).Info("listing nodes for providerID adoption failed, retrying", "host", h.Name, "err", err)
			return false, nil
		}
		for i := range nodes.Items {
			if node := &nodes.Items[i]; node.Spec.ProviderID != "" {
				providerID = node.Spec.ProviderID
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return "", fmt.Errorf("no node with InternalIP %s appeared within %s: %w", h.Spec.NodeIP(), adoptTimeout, err)
	}
	return providerID, nil
}

// runPhase executes one profile phase on a host in the host's exec mode: Raw
// renders the template and pipes it to `sudo bash -s`; Verified sends the
// signed, verbatim script to kpssh-shim, which verifies it before running.
func (p *DefaultProvider) runPhase(ctx context.Context, target *sshexec.Target, h *v1beta1.SSHHost, prof *v1beta1.SSHJoinProfile, phase string, pctx *profile.Context, env map[string]string) error {
	if h.Spec.ExecMode == v1beta1.ExecModeVerified {
		return runVerifiedPhase(ctx, p.execShim, *target, h, prof, phase, env)
	}
	script, err := profile.Render(prof, phase, pctx)
	if err != nil {
		return &configError{err}
	}
	_, err = p.exec(ctx, *target, script, env)
	return err
}

// runVerifiedPhase builds a signed envelope for a phase and runs it via the
// shim. The script is sent verbatim (byte-identical to what was signed), so it
// must be template-free and carry a signature; per-node values ride as params.
// The signature is verified against the host's trustedSigners before
// connecting — defense in depth and fail-fast; the host's shim re-verifies
// independently, so a compromised controller still cannot ship unsigned code.
func runVerifiedPhase(ctx context.Context, execShim sshexec.ShimRunner, target sshexec.Target, h *v1beta1.SSHHost, prof *v1beta1.SSHJoinProfile, phase string, env map[string]string) error {
	raw := profile.Script(prof, phase)
	if raw == "" {
		return &configError{fmt.Errorf("profile %s has no %s script", prof.Name, phase)}
	}
	if profile.HasTemplateActions(raw) {
		return &configError{fmt.Errorf("verified exec requires template-free scripts; profile %s %s uses Go template actions", prof.Name, phase)}
	}
	sig := profile.Signature(prof, phase)
	if sig == "" {
		return &configError{fmt.Errorf("verified host %s: profile %s has no signature for phase %s", h.Name, prof.Name, phase)}
	}
	// The CRD requires trustedSigners in Verified mode; the len guard only
	// tolerates objects stored before that rule existed.
	if len(h.Spec.TrustedSigners) > 0 {
		signers, err := sshexec.ParseTrustedSigners(h.Spec.TrustedSigners)
		if err != nil {
			return &configError{fmt.Errorf("host %s trustedSigners: %w", h.Name, err)}
		}
		if err := sshexec.VerifySSHSIG([]byte(raw), []byte(sig), signers, sshexec.DefaultSignNamespace); err != nil {
			return &configError{fmt.Errorf("controller-side signature check for profile %s %s: %w", prof.Name, phase, err)}
		}
	}
	target.ShimCommand = h.Spec.ShimCommand
	_, err := execShim(ctx, target, sshexec.Envelope{
		Phase:  phase,
		Script: []byte(raw),
		Sig:    []byte(sig),
		Params: env,
	})
	return err
}

func (p *DefaultProvider) join(ctx context.Context, nodeClass *v1beta1.SSHNodeClass, nodeClaim *karpv1.NodeClaim, h *v1beta1.SSHHost) error {
	log := ctrllog.FromContext(ctx)

	// Everything up to the first runPhase reads configuration (profile,
	// secrets, cluster info) without touching the host — failures here are
	// configErrors, so Create releases the host back to Available.
	prof := &v1beta1.SSHJoinProfile{}
	if err := p.kubeClient.Get(ctx, types.NamespacedName{Name: nodeClass.Spec.JoinProfileRef.Name}, prof); err != nil {
		return &configError{fmt.Errorf("getting join profile: %w", err)}
	}
	target, err := p.sshTarget(ctx, h)
	if err != nil {
		return &configError{err}
	}
	info, err := p.bootstrapProvider.ClusterInfo(ctx, nodeClass)
	if err != nil {
		return &configError{err}
	}

	pctx := &profile.Context{
		ClusterEndpoint:    info.Endpoint,
		ClusterCACert:      info.CACertB64,
		NodeName:           nodeClaim.Name,
		HostAddress:        h.Spec.Address,
		NodeAddress:        h.Spec.NodeIP(),
		NodeLabels:         renderNodeLabels(nodeClaim),
		RegisterWithTaints: renderTaints(nodeClaim),
		Vars:               nodeClass.Spec.Vars,
	}
	if ref := nodeClass.Spec.JoinSecretRef; ref != nil {
		secret, err := p.kubernetesInterface.CoreV1().Secrets(h.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return &configError{fmt.Errorf("getting joinSecretRef secret: %w", err)}
		}
		pctx.Secrets = map[string]string{}
		for k, v := range secret.Data {
			pctx.Secrets[k] = string(v)
		}
	}
	if nodeClass.Spec.ProviderIDSource != v1beta1.ProviderIDSourceAdopt {
		pctx.ProviderID = host.ProviderID(h)
	}

	env, err := pctx.Env()
	if err != nil {
		return &configError{err}
	}

	if h.Status.InstalledProfile != profile.InstalledMarker(prof) {
		log.Info("running install phase", "host", h.Name, "profile", prof.Name)
		ictx, cancel := context.WithTimeout(ctx, profile.Timeout(prof, profile.PhaseInstall, installTimeout))
		defer cancel()
		start := time.Now()
		err := p.runPhase(ictx, target, h, prof, profile.PhaseInstall, pctx, env)
		metrics.ObservePhase(profile.PhaseInstall, start, err)
		if err != nil {
			return redactPhaseErr(pctx, profile.PhaseInstall, err)
		}
		if err := p.hostProvider.SetInstalled(ctx, h, profile.InstalledMarker(prof)); err != nil {
			return err
		}
	}

	token, err := p.bootstrapProvider.CreateToken(ctx, tokenTTL, "karpenter-provider-ssh nodeclaim "+nodeClaim.Name)
	if err != nil {
		return err
	}
	pctx.BootstrapToken = token
	if env, err = pctx.Env(); err != nil {
		p.deleteToken(ctx, h, token)
		return err
	}

	// Record the token on the host before it is used: a controller crash
	// between here and the join leaves the token traceable (and deletable) by
	// the release path and the probe controller instead of orphaned in
	// kube-system until TTL — and then forever, tokencleaner being disabled by
	// default upstream.
	if err := p.hostProvider.SetBootstrapTokenID(ctx, h, bootstrap.TokenID(token)); err != nil {
		p.deleteToken(ctx, h, token)
		return fmt.Errorf("recording bootstrap token: %w", err)
	}

	jctx, cancel := context.WithTimeout(ctx, profile.Timeout(prof, profile.PhaseJoin, joinTimeout))
	defer cancel()
	start := time.Now()
	err = p.runPhase(jctx, target, h, prof, profile.PhaseJoin, pctx, env)
	metrics.ObservePhase(profile.PhaseJoin, start, err)
	if err != nil {
		// The token will never be used — remove it now rather than letting it
		// linger until TTL.
		p.deleteToken(ctx, h, token)
		return redactPhaseErr(pctx, profile.PhaseJoin, err)
	}
	// The token stays alive until the kubelet has traded it for a client
	// certificate: the probe controller deletes it once the node registers.
	log.Info("host joined", "host", h.Name, "node", nodeClaim.Name)
	return nil
}

// redactPhaseErr prefixes a phase error and redacts secret material from it:
// the executor quotes a stderr tail into the error, which ends up in NodeClaim
// events — and a script running under `set -x` traces its exported secrets to
// stderr. Redaction flattens the error to a string, so the configError
// classification is re-applied to the result.
func redactPhaseErr(pctx *profile.Context, phase string, err error) error {
	red := fmt.Errorf("%s phase: %s", phase, pctx.Redact(err.Error()))
	if isConfigError(err) {
		return &configError{red}
	}
	return red
}

// deleteToken removes a bootstrap token the join will not use, and forgets it
// on the host. Both failures are logged, never fatal: the caller is already on
// an error path, and a leftover token expires on its own.
func (p *DefaultProvider) deleteToken(ctx context.Context, h *v1beta1.SSHHost, token string) {
	log := ctrllog.FromContext(ctx)
	if err := p.bootstrapProvider.DeleteToken(ctx, token); err != nil {
		log.Error(err, "deleting unused bootstrap token", "host", h.Name)
	}
	if h.Status.BootstrapTokenID == "" {
		return
	}
	if err := p.hostProvider.SetBootstrapTokenID(ctx, h, ""); err != nil {
		log.Error(err, "clearing bootstrap token record", "host", h.Name)
	}
}

// Delete leaves and releases the host backing a NodeClaim. Idempotent;
// NodeClaimNotFoundError once this claim no longer holds a host — which is
// also how a Delete aimed at a host somebody else has since claimed ends, so
// karpenter's Delete-until-gone retries can never tear down a successor's node.
func (p *DefaultProvider) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	providerID := nodeClaim.Status.ProviderID
	log := ctrllog.FromContext(ctx).WithValues("providerID", providerID, "nodeClaim", nodeClaim.Name)

	h, err := p.hostProvider.ByProviderID(ctx, providerID)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return err
	}
	if h.Status.ClaimRef == nil {
		return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("host %s is not claimed", h.Name))
	}
	if !host.ClaimHeldBy(h, nodeClaim) {
		return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf(
			"host %s is claimed by NodeClaim %s, not %s", h.Name, h.Status.ClaimRef.Name, nodeClaim.Name))
	}

	leaveOK := false
	if h.Status.InstalledProfile != "" {
		if err := LeaveHost(ctx, p.kubeClient, p.kubernetesInterface, p.exec, p.execShim, h); err != nil {
			log.Error(err, "leave phase failed", "host", h.Name)
		} else {
			leaveOK = true
		}
	}

	// A join that never reached registration leaves its token behind.
	p.deleteBootstrapToken(ctx, h)

	// Release also clears status.providerID and the token record (single
	// patch). A lost CAS here means the host moved on under us: report the
	// conflict so karpenter retries Delete, which re-resolves and re-fences.
	if err := p.hostProvider.Release(ctx, h, leaveOK, "leave phase did not complete cleanly"); err != nil {
		return err
	}
	log.Info("host released", "host", h.Name, "leaveOK", leaveOK)
	return nil
}

// Get implements Provider.
func (p *DefaultProvider) Get(ctx context.Context, providerID string) (*Instance, error) {
	h, err := p.hostProvider.ByProviderID(ctx, providerID)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return nil, err
	}
	if h.Status.ClaimRef == nil {
		return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("host %s is not claimed", h.Name))
	}
	return p.toInstance(h, h.Status.ClaimRef.Name), nil
}

// List implements Provider.
func (p *DefaultProvider) List(ctx context.Context) ([]*Instance, error) {
	hosts, err := p.hostProvider.Claimed(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Instance, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, p.toInstance(h, h.Status.ClaimRef.Name))
	}
	return out, nil
}

func (p *DefaultProvider) toInstance(h *v1beta1.SSHHost, nodeName string) *Instance {
	pid := h.Status.ProviderID
	if pid == "" {
		pid = host.ProviderID(h)
	}
	return &Instance{
		ProviderID:       pid,
		HostName:         h.Name,
		NodeName:         nodeName,
		Class:            v1beta1.HostClass(h),
		Capacity:         h.Spec.Capacity,
		CreationTime:     h.CreationTimestamp.Time,
		InstalledProfile: h.Status.InstalledProfile,
	}
}

func (p *DefaultProvider) sshTarget(ctx context.Context, h *v1beta1.SSHHost) (*sshexec.Target, error) {
	creds, err := ReadSSHCredentials(ctx, p.kubernetesInterface, h)
	if err != nil {
		return nil, err
	}
	target := SSHTarget(h, creds)
	return &target, nil
}

func sortByPrice(its []*cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	out := slices.Clone(its)
	slices.SortStableFunc(out, func(a, b *cloudprovider.InstanceType) int {
		return cmp.Compare(cheapest(a), cheapest(b))
	})
	return out
}

// cheapest returns the lowest offering price; instance types with no
// offerings sort last.
func cheapest(it *cloudprovider.InstanceType) float64 {
	lowest := math.MaxFloat64
	for _, o := range it.Offerings {
		lowest = min(lowest, o.Price)
	}
	return lowest
}

// renderNodeLabels renders the NodeClaim labels the kubelet is allowed to
// self-set via --node-labels. The kubelet refuses to start when given labels
// in the kubernetes.io/k8s.io space outside the kubelet-owned namespaces —
// notably node-restriction.kubernetes.io, which is reserved for central
// controllers and rejected by NodeRestriction admission. Everything filtered
// here still lands on the Node: karpenter core syncs all NodeClaim labels at
// registration, fenced by the unregistered taint.
func renderNodeLabels(nodeClaim *karpv1.NodeClaim) string {
	parts := []string{}
	for k, v := range nodeClaim.Labels {
		if kubeletSelfLabel(k) {
			parts = append(parts, fmt.Sprintf("%s=%s", k, v))
		}
	}
	return strings.Join(parts, ",")
}

// kubeletSelfLabel mirrors the kubelet's --node-labels namespace policy
// (kubeletapis.IsKubeletLabel, minus the fixed well-known set, which core
// syncs at registration anyway): custom namespaces are fine; within
// kubernetes.io/k8s.io only the kubelet-owned namespaces are.
func kubeletSelfLabel(key string) bool {
	ns := ""
	if i := strings.IndexByte(key, '/'); i >= 0 {
		ns = key[:i]
	}
	inK8sSpace := ns == "kubernetes.io" || strings.HasSuffix(ns, ".kubernetes.io") ||
		ns == "k8s.io" || strings.HasSuffix(ns, ".k8s.io")
	if !inK8sSpace {
		return true
	}
	return ns == "kubelet.kubernetes.io" || strings.HasSuffix(ns, ".kubelet.kubernetes.io") ||
		ns == "node.kubernetes.io" || strings.HasSuffix(ns, ".node.kubernetes.io")
}

// renderTaints renders the NodeClaim taints for --register-with-taints,
// always prefixed with karpenter core's unregistered taint: core requires
// nodes to register with it (it fences the registration race — nothing
// schedules until core has synced labels/taints and removed it). Without it,
// a fast consolidation loop can delete the node before the workload lands.
func renderTaints(nodeClaim *karpv1.NodeClaim) string {
	parts := []string{fmt.Sprintf("%s:%s", karpv1.UnregisteredTaintKey, corev1.TaintEffectNoExecute)}
	for _, t := range nodeClaim.Spec.Taints {
		if t.Value != "" {
			parts = append(parts, fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect))
		} else {
			parts = append(parts, fmt.Sprintf("%s:%s", t.Key, t.Effect))
		}
	}
	return strings.Join(parts, ",")
}
