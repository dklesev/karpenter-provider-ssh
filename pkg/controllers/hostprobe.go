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

// Package controllers holds the provider-side reconcilers: host probing and
// node class readiness.
package controllers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/dklesev/karpenter-provider-ssh/internal/sshexec"
	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/metrics"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/bootstrap"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/instance"
)

// ProbeInterval and ProbeTimeout are exported because they set the pace an
// operator actually observes: after a host goes unreachable (a reboot, say),
// nothing gets noticed faster than one failed probe plus one interval. The e2e
// derives its reboot budget from them so retuning the cadence rescales the
// test instead of silently making it flaky.
const (
	ProbeInterval = 2 * time.Minute
	ProbeTimeout  = 30 * time.Second
)

const (
	claimedRecheck = 5 * time.Minute
	// tokenRecheck is the requeue while a claimed host still holds a live
	// bootstrap token: short, because the token dies as soon as the node it was
	// minted for registers.
	tokenRecheck = 30 * time.Second
	// conflictRetry is the requeue after losing an optimistic-lock race; the
	// next reconcile reads the fresh state.
	conflictRetry = time.Second
	// probeConcurrency bounds parallel reconciles. Probes block on SSH (up to
	// ProbeTimeout, leaves up to minutes), so with the controller-runtime
	// default of 1 a single dead host would stall probing of the whole pool.
	probeConcurrency = 8
)

// probeScript reads capacity + arch facts and the kubelet unit state,
// trivially portable. The kubelet line feeds the zombie guard: an unclaimed
// pool host must not run a kubelet.
const probeScript = `nproc
awk '/MemTotal/ {print $2}' /proc/meminfo
uname -m
systemctl is-active kubelet 2>/dev/null || true
`

// HostProbeReconciler owns host health probing and state hygiene. The
// clientset reads Secrets uncached — the cached client would demand a
// cluster-wide Secret watch the scoped RBAC does not grant.
type HostProbeReconciler struct {
	client.Client
	KubernetesInterface kubernetes.Interface
	// Exec runs scripts over SSH; swapped in tests.
	Exec sshexec.Runner
	// ExecShim runs verified (signed) scripts over SSH; swapped in tests.
	ExecShim sshexec.ShimRunner
	// Recorder emits SSHHost events (capacity drift, zombie guard actions).
	Recorder events.EventRecorder
	// Bootstrap deletes the join's bootstrap token once its node has
	// registered; defaulted in Register.
	Bootstrap bootstrap.Provider
}

// Reconcile probes hosts, pins host keys (TOFU), records capacity/arch, and
// releases stale claims whose NodeClaim vanished. Every status patch below
// re-triggers this reconciler through its own watch, so all probe outcomes
// are guarded by the staleness check — without it the controller would probe
// in a tight self-triggering loop instead of every ProbeInterval.
func (r *HostProbeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	host := &v1beta1.SSHHost{}
	if err := r.Get(ctx, req.NamespacedName, host); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	orig := host.DeepCopy()

	_, wantMaint := host.Annotations[v1beta1.MaintenanceAnnotation]
	switch {
	case wantMaint && host.Status.ClaimRef == nil && host.Status.State != v1beta1.HostStateMaintenance:
		host.Status.State = v1beta1.HostStateMaintenance
	case !wantMaint && host.Status.State == v1beta1.HostStateMaintenance:
		host.Status.State = v1beta1.HostStatePending
	}

	switch host.Status.State {
	case v1beta1.HostStateMaintenance:
		return r.patchStatus(ctx, orig, host, ctrl.Result{})

	case v1beta1.HostStateClaimed, v1beta1.HostStateInUse:
		// Stale-claim hygiene: the NodeClaim went away without a provider
		// Delete() (finalizer removed by hand), or its name was reused by a
		// different object — either way nothing holds this host any more.
		if ref := host.Status.ClaimRef; ref != nil {
			nc := &karpv1.NodeClaim{}
			err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, nc)
			stale := ""
			switch {
			case apierrors.IsNotFound(err):
				stale = "NodeClaim no longer exists"
			case err != nil:
				// Transient API error — retry with backoff, don't treat the
				// claim as alive on no evidence.
				return ctrl.Result{}, err
			case ref.UID != "" && nc.UID != "" && ref.UID != nc.UID:
				stale = "NodeClaim name was reused by a different object"
			}
			if stale != "" {
				log.Info("releasing host with stale claimRef", "nodeClaim", ref.Name, "reason", stale)
				r.deleteBootstrapToken(ctx, host)
				host.Status.ClaimRef = nil
				host.Status.ProviderID = ""
				host.Status.BootstrapTokenID = ""
				host.Status.State = v1beta1.HostStatePending
				return r.patchStatus(ctx, orig, host, ctrl.Result{RequeueAfter: conflictRetry})
			}
		}

		// Bootstrap-token hygiene: the token minted for this join dies with the
		// node's registration — by then the kubelet holds a client certificate
		// and never needs it again. Nothing else collects it: tokencleaner is
		// disabled by default upstream, so an uncollected token Secret would
		// outlive its TTL in kube-system forever.
		//
		// Gated on providerID: Create() records the token before the join and
		// the providerID after it, so an empty providerID means the launch is
		// still in flight. Collecting now would patch the status mid-Create and
		// steal its optimistic lock — with a pre-registered Node that race is
		// deterministic and livelocks the launch.
		if host.Status.BootstrapTokenID != "" && host.Status.ProviderID != "" && r.Bootstrap != nil {
			node, err := r.nodeByInternalIP(ctx, host.Spec.NodeIP())
			if err != nil {
				return ctrl.Result{}, err
			}
			if node == nil {
				return r.patchStatus(ctx, orig, host, ctrl.Result{RequeueAfter: tokenRecheck})
			}
			if err := r.Bootstrap.DeleteTokenByID(ctx, host.Status.BootstrapTokenID); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting spent bootstrap token: %w", err)
			}
			log.V(1).Info("bootstrap token collected after node registration",
				"host", host.Name, "node", node.Name)
			host.Status.BootstrapTokenID = ""
		}
		return r.patchStatus(ctx, orig, host, ctrl.Result{RequeueAfter: claimedRecheck})

	default: // Pending (incl. ""), Available, Unhealthy, Leaving → probe
		// Staleness guard: skip if this spec generation was probed recently.
		if host.Status.ObservedGeneration == host.Generation && host.Status.LastProbeTime != nil {
			if since := time.Since(host.Status.LastProbeTime.Time); since < ProbeInterval {
				return ctrl.Result{RequeueAfter: ProbeInterval - since}, nil
			}
		}

		log.V(1).Info("probing host", "host", host.Name, "state", host.Status.State)
		probeStart := time.Now()
		res, err := r.probe(ctx, host)
		metrics.ObserveProbe(probeStart, err)
		now := metav1.Now()
		host.Status.LastProbeTime = &now
		host.Status.ObservedGeneration = host.Generation
		if err != nil {
			host.Status.State = v1beta1.HostStateUnhealthy
			host.Status.LastProbeError = err.Error()
			return r.patchStatus(ctx, orig, host, ctrl.Result{RequeueAfter: ProbeInterval})
		}

		if host.Status.HostKeyFingerprint == "" {
			host.Status.HostKeyFingerprint = res.fingerprint
		}
		host.Status.ObservedCapacity = corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(res.cpus, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(res.memKB*1024, resource.BinarySI),
		}
		host.Status.ObservedArch = res.arch
		r.warnOnCapacityDrift(orig, host)

		// Zombie guard: a kubelet on an unclaimed pool host means the host is
		// (or is about to be) a cluster member nobody pays attention to — the
		// classic case is a reboot restarting an enabled kubelet with warm
		// credentials, silently rejoining and re-opening the billing window.
		if res.kubeletActive {
			// Persist the probe facts first; handleZombie then continues from
			// the resourceVersion this patch returns (it must NOT re-read).
			if err := r.Status().Patch(ctx, host, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{RequeueAfter: conflictRetry}, nil
				}
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			return r.handleZombie(ctx, host)
		}

		host.Status.LastProbeError = ""
		host.Status.State = v1beta1.HostStateAvailable
		return r.patchStatus(ctx, orig, host, ctrl.Result{RequeueAfter: ProbeInterval})
	}
}

// warnOnCapacityDrift emits one warning event when the probed capacity newly
// falls short of what spec.capacity promises.
func (r *HostProbeReconciler) warnOnCapacityDrift(orig, host *v1beta1.SSHHost) {
	if r.Recorder == nil {
		return
	}
	now := capacityShortfall(host.Spec.Capacity, host.Status.ObservedCapacity)
	if len(now) == 0 {
		return
	}
	if was := capacityShortfall(orig.Spec.Capacity, orig.Status.ObservedCapacity); len(was) > 0 {
		return // already warned while the drift persists
	}
	r.Recorder.Eventf(host, nil, corev1.EventTypeWarning, "CapacityDrift", "Probe",
		"observed capacity below spec.capacity: %s", strings.Join(now, ", "))
}

// capacityShortfall lists resources whose observed value is below spec.
func capacityShortfall(spec, observed corev1.ResourceList) []string {
	if len(observed) == 0 {
		return nil
	}
	short := []string{}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		want, wantOK := spec[name]
		got, gotOK := observed[name]
		if wantOK && gotOK && got.Cmp(want) < 0 {
			short = append(short, fmt.Sprintf("%s: observed %s < spec %s", name, got.String(), want.String()))
		}
	}
	return short
}

// handleZombie disconnects an unclaimed host that runs a kubelet. If the host
// backs a Node of this cluster (matched by InternalIP), the guard runs the
// profile's leave (stops + disables kubelet) and then deletes the Node object
// — that order keeps kubelet from re-creating it. A kubelet without a matching
// Node is not touched: it may belong to another cluster, which is an operator
// problem, not something to destroy from here.
//
// host carries the resourceVersion returned by the probe-facts patch, so the
// Leaving CAS below runs against the freshest known state. Deliberately NO
// cache re-read here: the informer will not have ingested our own patch yet,
// and a CAS based on that stale read would conflict deterministically —
// probing every interval and never progressing. A claim racing this guard
// bumps the resourceVersion and fails the CAS instead.
func (r *HostProbeReconciler) handleZombie(ctx context.Context, host *v1beta1.SSHHost) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Defensive: the probe branch only runs for unclaimed states, but a claim
	// racing the probe may already be visible on our snapshot.
	if host.Status.ClaimRef != nil {
		log.V(1).Info("zombie guard: host claimed, backing off", "host", host.Name)
		return ctrl.Result{RequeueAfter: ProbeInterval}, nil
	}

	node, err := r.nodeByInternalIP(ctx, host.Spec.NodeIP())
	if err != nil {
		return ctrl.Result{}, err
	}
	// No Node AND no install marker: the kubelet is not of our making —
	// possibly a foreign cluster's member. Park, never destroy.
	// With our install marker the host is ours to leave even when the Node
	// is momentarily absent: karpenter core garbage-collects orphaned Nodes
	// on its own (without running leave), so kubelet-running-with-no-Node is
	// a normal transient of the very failure mode this guard exists for.
	if node == nil && host.Status.InstalledProfile == "" {
		orig := host.DeepCopy()
		host.Status.State = v1beta1.HostStateUnhealthy
		host.Status.LastProbeError = "kubelet active on unclaimed host that this provider never installed — foreign membership? run leave/uninstall manually or remove the host from the pool"
		r.event(host, corev1.EventTypeWarning, "ForeignKubelet", "Park", host.Status.LastProbeError)
		metrics.RecordZombieAction(metrics.ZombieActionParkedForeign)
		return r.patchStatus(ctx, orig, host, ctrl.Result{RequeueAfter: ProbeInterval})
	}

	// Fence the leave: while Leaving, the host is not claimable, so a claim
	// racing this guard cannot have its fresh join torn down. The optimistic
	// lock makes the transition a CAS — a concurrent writer wins the race and
	// this reconcile backs off.
	orig := host.DeepCopy()
	host.Status.State = v1beta1.HostStateLeaving
	if err := r.Status().Patch(ctx, host, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			log.Info("zombie guard: lost Leaving CAS to a concurrent writer, backing off", "host", host.Name)
			return ctrl.Result{RequeueAfter: conflictRetry}, nil
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("zombie membership detected: kubelet on unclaimed host — running leave",
		"host", host.Name, "nodeFound", node != nil)
	r.event(host, corev1.EventTypeWarning, "ZombieKubelet", "Leave",
		fmt.Sprintf("kubelet active on unclaimed host (node found: %t) — running leave", node != nil))

	if err := instance.LeaveHost(ctx, r.Client, r.KubernetesInterface, r.Exec, r.ExecShim, host); err != nil {
		metrics.RecordZombieAction(metrics.ZombieActionLeaveFailed)
		// The most operationally urgent state this controller can reach — a
		// host running a rogue kubelet that we could not stop, still on the
		// invoice — and it was the one zombie outcome with neither an event
		// nor a log line, discoverable only by kubectl-ing the CR.
		log.Error(err, "zombie leave failed — host still running an unclaimed kubelet", "host", host.Name)
		r.event(host, corev1.EventTypeWarning, "ZombieLeaveFailed", "Leave",
			fmt.Sprintf("leave failed on an unclaimed host with an active kubelet: %v", err))
		orig := host.DeepCopy()
		host.Status.State = v1beta1.HostStateUnhealthy
		host.Status.LastProbeError = fmt.Sprintf("zombie leave failed: %v", err)
		return r.patchStatus(ctx, orig, host, ctrl.Result{RequeueAfter: ProbeInterval})
	}
	// kubelet is down and disabled; now the Node object can go without being
	// re-created. Billing (EKS Hybrid) follows the Node object.
	if node != nil {
		if err := r.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		log.Info("zombie node removed", "host", host.Name, "node", node.Name)
	}
	r.event(host, corev1.EventTypeNormal, "ZombieLeft", "Leave", "zombie membership disconnected, host available again")
	metrics.RecordZombieAction(metrics.ZombieActionLeft)

	orig = host.DeepCopy()
	host.Status.LastProbeError = ""
	host.Status.State = v1beta1.HostStateAvailable
	return r.patchStatus(ctx, orig, host, ctrl.Result{RequeueAfter: ProbeInterval})
}

func (r *HostProbeReconciler) event(host *v1beta1.SSHHost, eventType, reason, action, message string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(host, nil, eventType, reason, action, "%s", message)
	}
}

// nodeByInternalIP returns the Node advertising the given InternalIP, or nil.
func (r *HostProbeReconciler) nodeByInternalIP(ctx context.Context, ip string) (*corev1.Node, error) {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes, client.MatchingFields{v1beta1.NodeInternalIPIndex: ip}); err != nil {
		return nil, err
	}
	if len(nodes.Items) == 0 {
		return nil, nil
	}
	return &nodes.Items[0], nil
}

type probeResult struct {
	cpus          int64
	memKB         int64
	arch          string
	fingerprint   string
	kubeletActive bool
}

func (r *HostProbeReconciler) probe(ctx context.Context, host *v1beta1.SSHHost) (*probeResult, error) {
	creds, err := instance.ReadSSHCredentials(ctx, r.KubernetesInterface, host)
	if err != nil {
		return nil, err
	}

	pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	target := instance.SSHTarget(host, creds)
	var out *sshexec.Result
	if host.Spec.ExecMode == v1beta1.ExecModeVerified {
		// Verified hosts force-command the shim; probe is its built-in,
		// unsigned read-only op.
		target.ShimCommand = host.Spec.ShimCommand
		out, err = r.ExecShim(pctx, target, sshexec.Envelope{Phase: sshexec.PhaseProbe})
	} else {
		out, err = r.Exec(pctx, target, probeScript, nil)
	}
	if err != nil {
		return nil, err
	}

	return parseProbeOutput(out.Stdout, out.HostKeyFingerprint), nil
}

// parseProbeOutput maps probeScript's stdout lines (nproc, MemTotal kB,
// uname -m, kubelet unit state) onto a probeResult.
func parseProbeOutput(stdout, fingerprint string) *probeResult {
	fields := strings.Fields(strings.TrimSpace(stdout))
	res := &probeResult{fingerprint: fingerprint}
	if len(fields) >= 3 {
		res.cpus, _ = strconv.ParseInt(fields[0], 10, 64)
		res.memKB, _ = strconv.ParseInt(fields[1], 10, 64)
		res.arch = normalizeArch(fields[2])
	}
	if len(fields) >= 4 {
		res.kubeletActive = fields[3] == "active"
	}
	return res
}

func normalizeArch(m string) string {
	switch m {
	case "x86_64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return m
}

// deleteBootstrapToken drops the token minted for a join that will never
// finish. Failures are logged, not fatal: the release must proceed, and a
// leftover token expires on its own.
func (r *HostProbeReconciler) deleteBootstrapToken(ctx context.Context, host *v1beta1.SSHHost) {
	if host.Status.BootstrapTokenID == "" || r.Bootstrap == nil {
		return
	}
	if err := r.Bootstrap.DeleteTokenByID(ctx, host.Status.BootstrapTokenID); err != nil {
		ctrllog.FromContext(ctx).Error(err, "deleting bootstrap token of a stale claim",
			"host", host.Name, "tokenID", host.Status.BootstrapTokenID)
	}
}

// patchStatus applies a status patch with an optimistic lock: state here is a
// small CAS-driven machine shared with the instance provider's claim path, so
// last-write-wins merges could resurrect a state another actor just left.
// Lost races are logged and requeued for a fresh read — never swallowed
// silently, so a persistent conflict is visible in the logs. Unchanged status
// is not written at all: every claimed host is rechecked on a timer, and the
// apiserver should not hear about a recheck that found nothing to say.
func (r *HostProbeReconciler) patchStatus(ctx context.Context, orig, host *v1beta1.SSHHost, result ctrl.Result) (ctrl.Result, error) {
	setReadyCondition(host)
	if equality.Semantic.DeepEqual(orig.Status, host.Status) {
		return result, nil
	}
	if err := r.Status().Patch(ctx, host, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			ctrllog.FromContext(ctx).Info("status patch lost an optimistic-lock race, retrying",
				"host", host.Name, "state", host.Status.State)
			return ctrl.Result{RequeueAfter: conflictRetry}, nil
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return result, nil
}

// setReadyCondition mirrors State onto the Ready condition. It is derived, not
// independently tracked, so the two can never disagree: State is what the claim
// lock CASes on, Ready is the same fact in the shape the ecosystem expects
// (`kubectl wait --for=condition=Ready`). Called from patchStatus so every
// probe write carries it, and the semantic-equality guard there keeps an
// unchanged condition from generating a no-op PATCH.
func setReadyCondition(host *v1beta1.SSHHost) {
	cond := metav1.Condition{
		Type:               v1beta1.HostConditionReady,
		ObservedGeneration: host.Generation,
	}
	switch host.Status.State {
	case v1beta1.HostStateUnhealthy:
		cond.Status = metav1.ConditionFalse
		cond.Reason = v1beta1.HostReadyReasonProbeFailed
		cond.Message = host.Status.LastProbeError
	case v1beta1.HostStateMaintenance:
		cond.Status = metav1.ConditionFalse
		cond.Reason = v1beta1.HostReadyReasonMaintenance
		cond.Message = "host is annotated for maintenance and will not be claimed"
	case v1beta1.HostStatePending, "":
		cond.Status = metav1.ConditionUnknown
		cond.Reason = v1beta1.HostReadyReasonPending
		cond.Message = "no successful probe yet"
	default: // Available, Claimed, InUse, Leaving — the probe reached it.
		cond.Status = metav1.ConditionTrue
		cond.Reason = v1beta1.HostReadyReasonProbeSucceeded
	}
	if cond.Message == "" {
		cond.Message = string(host.Status.State)
	}
	meta.SetStatusCondition(&host.Status.Conditions, cond)
}

// Register wires the reconciler.
func (r *HostProbeReconciler) Register(_ context.Context, mgr ctrl.Manager) error {
	if r.Exec == nil {
		r.Exec = sshexec.Run
	}
	if r.ExecShim == nil {
		r.ExecShim = sshexec.RunShim
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("kpssh-hostprobe")
	}
	if r.Bootstrap == nil {
		r.Bootstrap = bootstrap.NewDefaultProvider(r.KubernetesInterface)
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("hostprobe").
		For(&v1beta1.SSHHost{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: probeConcurrency}).
		Complete(r)
}
