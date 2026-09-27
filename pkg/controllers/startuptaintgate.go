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

package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/metrics"
)

// nodeClaimAPIVersion is the owner-reference apiVersion karpenter core writes
// onto registered Nodes (apis.Group + the served version); core exports no
// GroupVersion value for it.
var nodeClaimAPIVersion = schema.GroupVersion{Group: apis.Group, Version: "v1"}.String()

// gateRecheck bounds how long a node stays tainted after its gate condition
// became true but the triggering event was missed (cache hiccup, pod Ready
// flip during a leader failover). Startup taints block scheduling, so this
// is the worst-case latency an operator observes — kept short.
const gateRecheck = 30 * time.Second

// StartupTaintGateReconciler removes NodePool startupTaints from registered
// Nodes once the SSHNodeClass gate condition for that taint holds.
//
// Karpenter core stamps NodePool startupTaints onto the Node at registration
// and holds the NodeClaim's Initialized condition until they are gone — but
// core expects the tainted-for DaemonSet to remove its own taint (the
// node.cilium.io/agent-not-ready pattern). Components that cannot do that
// (managed addons, credential agents) need someone else to open the gate:
// this reconciler, on an observable condition the node class declares.
//
// Invariant: a taint is removed only if it is (a) named by a gate on the
// node's SSHNodeClass AND (b) listed in the owning NodeClaim's
// spec.startupTaints. The class alone cannot strip a taint the pool did not
// declare as a startup taint — so a gate can never touch a permanent taint
// (NodePool taints, kubelet-registered taints, controller taints).
type StartupTaintGateReconciler struct {
	client.Client
	// Recorder emits Node events (StartupTaintRemoved); defaulted in Register.
	Recorder events.EventRecorder
}

// Reconcile evaluates every gate of the node's class against the live Node
// and its pods, and patches matured taints away in one write.
func (r *StartupTaintGateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !node.DeletionTimestamp.IsZero() || len(node.Spec.Taints) == 0 {
		return ctrl.Result{}, nil
	}

	nodeClaim, err := r.nodeClaimFor(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}
	if nodeClaim == nil || len(nodeClaim.Spec.StartupTaints) == 0 {
		// Not a karpenter node, another cloudprovider's node, or a pool with
		// no startup taints: nothing we may touch.
		return ctrl.Result{}, nil
	}
	nodeClass, err := r.nodeClassFor(ctx, nodeClaim)
	if err != nil {
		return ctrl.Result{}, err
	}
	if nodeClass == nil || len(nodeClass.Spec.StartupTaintGates) == 0 {
		return ctrl.Result{}, nil
	}

	log := ctrllog.FromContext(ctx).WithValues("nodeClaim", nodeClaim.Name, "nodeClass", nodeClass.Name)
	remove := map[string]bool{}
	pending := 0
	for i := range nodeClass.Spec.StartupTaintGates {
		gate := &nodeClass.Spec.StartupTaintGates[i]
		if !hasTaintKey(node.Spec.Taints, gate.TaintKey) {
			continue // already gone (or never applied)
		}
		if !hasTaintKey(nodeClaim.Spec.StartupTaints, gate.TaintKey) {
			// Declared as a gate but not as a startup taint: the pool means it
			// to stay. Say so once per reconcile at debug level — a permanent
			// taint that happens to share a key with a gate is a config bug
			// worth surfacing, but never worth acting on.
			log.V(1).Info("gate taint is not a NodeClaim startupTaint; leaving it", "taintKey", gate.TaintKey)
			continue
		}
		open, err := r.evaluate(ctx, node, &gate.RemoveWhen)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("evaluating gate %q: %w", gate.TaintKey, err)
		}
		if open {
			remove[gate.TaintKey] = true
		} else {
			pending++
		}
	}

	if len(remove) > 0 {
		if err := r.removeTaints(ctx, node, remove); err != nil {
			// A lost optimistic-lock race re-enters through the Node watch
			// (the winning write is an update event); no extra requeue.
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	if pending > 0 {
		return ctrl.Result{RequeueAfter: gateRecheck}, nil
	}
	return ctrl.Result{}, nil
}

// evaluate reports whether one gate condition currently holds for the node.
func (r *StartupTaintGateReconciler) evaluate(ctx context.Context, node *corev1.Node, cond *v1beta1.StartupTaintCondition) (bool, error) {
	switch {
	case cond.PodsReady != nil:
		return r.podsReady(ctx, node, cond.PodsReady)
	case cond.NodeCondition != nil:
		for _, c := range node.Status.Conditions {
			if string(c.Type) == cond.NodeCondition.Type {
				return c.Status == cond.NodeCondition.StatusOrDefault(), nil
			}
		}
		return false, nil
	default:
		// CEL rejects this at admission; an object that predates the rule
		// (or bypassed it) must not open the gate by accident.
		return false, fmt.Errorf("removeWhen sets no condition")
	}
}

// podsReady counts Ready pods matching the selector on this node. The list is
// namespace+selector scoped and filtered on nodeName in Go on purpose: it
// stays correct without the pod spec.nodeName field index karpenter core
// happens to register (an implicit dependency a fake client or a second
// manager would not reproduce), and the matched set — DaemonSet pods of one
// component — is one pod per node.
func (r *StartupTaintGateReconciler) podsReady(ctx context.Context, node *corev1.Node, cond *v1beta1.PodsReadyCondition) (bool, error) {
	selector, err := metav1.LabelSelectorAsSelector(&cond.Selector)
	if err != nil {
		return false, fmt.Errorf("podsReady selector: %w", err)
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(cond.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return false, fmt.Errorf("listing pods: %w", err)
	}
	ready := int32(0)
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != node.Name || !p.DeletionTimestamp.IsZero() {
			continue
		}
		if isPodReady(p) {
			ready++
		}
	}
	return ready >= cond.MinReadyOrDefault(), nil
}

// removeTaints drops the given keys from the Node in one optimistic-lock
// patch, then records the event and the gate latency per key.
func (r *StartupTaintGateReconciler) removeTaints(ctx context.Context, node *corev1.Node, keys map[string]bool) error {
	stored := node.DeepCopy()
	kept := make([]corev1.Taint, 0, len(node.Spec.Taints))
	for _, t := range node.Spec.Taints {
		if !keys[t.Key] {
			kept = append(kept, t)
		}
	}
	node.Spec.Taints = kept
	if err := r.Patch(ctx, node, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			ctrllog.FromContext(ctx).V(1).Info("startup taint patch lost an optimistic-lock race; retrying via watch")
			return nil
		}
		return fmt.Errorf("removing startup taints: %w", err)
	}
	for key := range keys {
		ctrllog.FromContext(ctx).Info("startup taint removed", "taintKey", key)
		metrics.ObserveStartupTaintGate(key, node.CreationTimestamp.Time)
		if r.Recorder != nil {
			r.Recorder.Eventf(node, nil, corev1.EventTypeNormal, "StartupTaintRemoved", "GateOpened",
				"startup taint %s removed: gate condition met", key)
		}
	}
	return nil
}

// nodeClaimFor resolves the Node's owning NodeClaim through the owner
// reference karpenter core sets at registration — the same write that stamps
// the startup taints, so a tainted karpenter node always carries it.
func (r *StartupTaintGateReconciler) nodeClaimFor(ctx context.Context, node *corev1.Node) (*karpv1.NodeClaim, error) {
	for _, ref := range node.OwnerReferences {
		if ref.Kind != "NodeClaim" || ref.APIVersion != nodeClaimAPIVersion {
			continue
		}
		nc := &karpv1.NodeClaim{}
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, nc); err != nil {
			return nil, client.IgnoreNotFound(err)
		}
		return nc, nil
	}
	return nil, nil
}

// nodeClassFor returns the SSHNodeClass a NodeClaim references, or nil when
// the claim belongs to another cloudprovider (coexistence) or the class is gone.
func (r *StartupTaintGateReconciler) nodeClassFor(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*v1beta1.SSHNodeClass, error) {
	ref := nodeClaim.Spec.NodeClassRef
	if ref == nil || ref.Group != v1beta1.GroupVersion.Group || ref.Kind != v1beta1.SSHNodeClassKind {
		return nil, nil
	}
	nc := &v1beta1.SSHNodeClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, nc); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return nc, nil
}

// Register wires the reconciler: Nodes drive it; Pods and SSHNodeClasses
// fan in because the gate reads both.
func (r *StartupTaintGateReconciler) Register(_ context.Context, mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("kpssh-startup-taint-gate")
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("startuptaintgate").
		// Only tainted karpenter-owned nodes can have work here. Node status
		// heartbeats on such nodes still reconcile, but that is a handful of
		// cache reads per node per heartbeat, never an API write.
		For(&corev1.Node{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			n, ok := o.(*corev1.Node)
			return ok && len(n.Spec.Taints) > 0 && ownedByNodeClaim(n)
		}))).
		// A pod turning Ready is THE event a podsReady gate waits for; every
		// other pod update is noise (karpenter core already caches all pods,
		// so the watch costs no extra informer).
		Watches(&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.nodeForPod),
			builder.WithPredicates(podReadyChanged())).
		// Editing gates on a class must re-evaluate its live nodes.
		Watches(&v1beta1.SSHNodeClass{}, handler.EnqueueRequestsFromMapFunc(r.nodesForNodeClass)).
		Complete(r)
}

// nodeForPod maps a pod event to its node.
func (r *StartupTaintGateReconciler) nodeForPod(_ context.Context, o client.Object) []reconcile.Request {
	p, ok := o.(*corev1.Pod)
	if !ok || p.Spec.NodeName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: p.Spec.NodeName}}}
}

// nodesForNodeClass maps a class to the nodes of its NodeClaims via the
// nodeClassRef index karpenter core registers and NodeClaim.status.nodeName
// (set at registration, exactly when startup taints appear).
func (r *StartupTaintGateReconciler) nodesForNodeClass(ctx context.Context, o client.Object) []reconcile.Request {
	nc, ok := o.(*v1beta1.SSHNodeClass)
	if !ok || len(nc.Spec.StartupTaintGates) == 0 {
		return nil
	}
	claims := &karpv1.NodeClaimList{}
	if err := r.List(ctx, claims, nodeclaimutils.ForNodeClass(nc)); err != nil {
		ctrllog.FromContext(ctx).Error(err, "listing NodeClaims to fan out a node class event")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(claims.Items))
	for i := range claims.Items {
		if name := claims.Items[i].Status.NodeName; name != "" {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		}
	}
	return reqs
}

// podReadyChanged passes creates/deletes and only those updates where the
// pod's Ready condition flipped.
func podReadyChanged() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, ok1 := e.ObjectOld.(*corev1.Pod)
			newPod, ok2 := e.ObjectNew.(*corev1.Pod)
			if !ok1 || !ok2 {
				return false
			}
			return isPodReady(oldPod) != isPodReady(newPod)
		},
	}
}

func ownedByNodeClaim(n *corev1.Node) bool {
	for _, ref := range n.OwnerReferences {
		if ref.Kind == "NodeClaim" && ref.APIVersion == nodeClaimAPIVersion {
			return true
		}
	}
	return false
}

func hasTaintKey(taints []corev1.Taint, key string) bool {
	for _, t := range taints {
		if t.Key == key {
			return true
		}
	}
	return false
}

func isPodReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
