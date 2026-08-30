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

	"github.com/awslabs/operatorpkg/status"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"

	"github.com/dklesev/karpenter-provider-ssh/internal/profile"
	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/host"
)

// NodeClassReconciler reports SSHNodeClass readiness: profile exists and the
// selector matches at least one registered host.
type NodeClassReconciler struct {
	client.Client
	HostProvider host.Provider
}

// Reconcile sets the Ready condition.
func (r *NodeClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	nc := &v1beta1.SSHNodeClass{}
	if err := r.Get(ctx, req.NamespacedName, nc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !nc.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, nc)
	}
	if controllerutil.AddFinalizer(nc, v1beta1.TerminationFinalizer) {
		if err := r.Update(ctx, nc); err != nil {
			return ctrl.Result{}, err
		}
	}
	orig := nc.DeepCopy()

	prof := &v1beta1.SSHJoinProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: nc.Spec.JoinProfileRef.Name}, prof); err != nil {
		if !apierrors.IsNotFound(err) {
			// Transient API/cache error: retry with backoff instead of
			// sticking a misleading ProfileNotFound onto the nodeclass.
			return ctrl.Result{}, err
		}
		nc.StatusConditions().SetFalse(status.ConditionReady, "ProfileNotFound",
			fmt.Sprintf("SSHJoinProfile %q not found", nc.Spec.JoinProfileRef.Name))
		return ctrl.Result{}, r.patchStatusIfChanged(ctx, nc, orig)
	}
	// A profile whose scripts do not parse — or whose leave needs data only the
	// join path has — fails at claim time otherwise, when a host is already
	// half-joined and the zombie guard cannot disconnect it.
	if err := profile.Validate(prof); err != nil {
		nc.StatusConditions().SetFalse(status.ConditionReady, "ProfileInvalid", err.Error())
		return ctrl.Result{}, r.patchStatusIfChanged(ctx, nc, orig)
	}

	// An unparsable selector is the node class's own bug (terminal, report it);
	// a failing List is not (transient, back off). Collapsing the two would
	// stick NotReady onto a healthy class on any cache hiccup.
	if _, err := metav1.LabelSelectorAsSelector(nc.Spec.HostSelector); err != nil {
		nc.StatusConditions().SetFalse(status.ConditionReady, "SelectorError", err.Error())
		return ctrl.Result{}, r.patchStatusIfChanged(ctx, nc, orig)
	}
	hosts, err := r.HostProvider.All(ctx, nc.Spec.HostSelector)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(hosts) == 0 {
		nc.StatusConditions().SetFalse(status.ConditionReady, "NoHosts", "no SSHHosts match the selector")
		return ctrl.Result{}, r.patchStatusIfChanged(ctx, nc, orig)
	}

	nc.StatusConditions().SetTrue(status.ConditionReady)
	return ctrl.Result{}, r.patchStatusIfChanged(ctx, nc, orig)
}

// finalize blocks deletion while NodeClaims still reference this nodeclass:
// removing the class first would strand their Delete path (leave/release
// need the class to resolve hosts and credentials).
func (r *NodeClassReconciler) finalize(ctx context.Context, nc *v1beta1.SSHNodeClass) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(nc, v1beta1.TerminationFinalizer) {
		return ctrl.Result{}, nil
	}
	// Indexed by spec.nodeClassRef.{group,kind,name} — karpenter core registers
	// those indexes on the manager (operator.setupIndexers), so this is a
	// keyed lookup rather than "list every NodeClaim in the cluster and filter
	// in Go". Register() watches NodeClaims, so the last release wakes us
	// immediately instead of on a poll.
	claims := &karpv1.NodeClaimList{}
	if err := r.List(ctx, claims, nodeclaimutils.ForNodeClass(nc)); err != nil {
		return ctrl.Result{}, err
	}
	if len(claims.Items) > 0 {
		ctrl.LoggerFrom(ctx).Info("nodeclass deletion blocked by referencing NodeClaims",
			"nodeClass", nc.Name, "nodeClaims", len(claims.Items))
		return ctrl.Result{}, nil
	}
	controllerutil.RemoveFinalizer(nc, v1beta1.TerminationFinalizer)
	return ctrl.Result{}, r.Update(ctx, nc)
}

// patchStatusIfChanged skips the write when the status is semantically
// unchanged: every SSHHost status write (each probe, every ~2min per host)
// fans out to all nodeclasses, which would otherwise stream no-op status
// PATCHes at the apiserver.
func (r *NodeClassReconciler) patchStatusIfChanged(ctx context.Context, nc, orig *v1beta1.SSHNodeClass) error {
	if equality.Semantic.DeepEqual(orig.Status, nc.Status) {
		return nil
	}
	return r.Status().Patch(ctx, nc, client.MergeFrom(orig))
}

// Register wires the reconciler. Readiness depends on objects the nodeclass
// does not own — hosts (selector match) and the join profile — so both are
// watched and fan out to every nodeclass (counts are tiny).
func (r *NodeClassReconciler) Register(_ context.Context, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("nodeclass").
		For(&v1beta1.SSHNodeClass{}).
		// LabelChanged, not every update: readiness reads only a host's
		// existence and its labels (HostProvider.All filters on
		// namespace+selector and never looks at status). Without the
		// predicate, every probe heartbeat — one status write per host per
		// ProbeInterval — fans out to EVERY nodeclass, and each of those
		// reconciles re-lists all hosts and re-parses up to 4x256KiB of
		// profile scripts. On a large pool that is a permanent, entirely
		// wasted spin. The zero value is Create+Delete=true, Update=labels
		// only, which is exactly this reconciler's dependency set.
		Watches(&v1beta1.SSHHost{},
			handler.EnqueueRequestsFromMapFunc(r.allNodeClasses),
			builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Watches(&v1beta1.SSHJoinProfile{}, handler.EnqueueRequestsFromMapFunc(r.allNodeClasses)).
		// The termination finalizer waits on referencing NodeClaims; watching
		// them means the last one going away wakes finalize() directly.
		Watches(&karpv1.NodeClaim{}, handler.EnqueueRequestsFromMapFunc(r.nodeClassForClaim)).
		Complete(r)
}

// nodeClassForClaim maps a NodeClaim to the SSHNodeClass it references, and to
// nothing at all when the claim belongs to another cloudprovider (an
// EC2NodeClass on a shared cluster). Karpenter core ships NodeClassEventHandler
// for the opposite direction — NodeClass -> NodeClaim requests — which would
// enqueue nodeclaim names into this nodeclass reconciler.
func (r *NodeClassReconciler) nodeClassForClaim(_ context.Context, o client.Object) []reconcile.Request {
	nc, ok := o.(*karpv1.NodeClaim)
	if !ok {
		return nil
	}
	ref := nc.Spec.NodeClassRef
	if ref == nil || ref.Group != v1beta1.GroupVersion.Group || ref.Kind != v1beta1.SSHNodeClassKind {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: ref.Name}}}
}

// allNodeClasses maps any host/profile event to all SSHNodeClasses.
func (r *NodeClassReconciler) allNodeClasses(ctx context.Context, _ client.Object) []reconcile.Request {
	list := &v1beta1.SSHNodeClassList{}
	if err := r.List(ctx, list); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing node classes to fan out a host/profile event")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: list.Items[i].Name}})
	}
	return reqs
}
