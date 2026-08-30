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
	"testing"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/host"
)

func newNodeClassReconciler(t *testing.T, objs ...runtime.Object) (*NodeClassReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithStatusSubresource(&v1beta1.SSHNodeClass{}, &v1beta1.SSHHost{}).
		WithRuntimeObjects(objs...).
		// finalize() lists NodeClaims through nodeclaimutils.ForNodeClass,
		// i.e. client.MatchingFields. In production karpenter core registers
		// these on the manager (operator.setupIndexers); a fake client has no
		// indexes unless asked, and an unregistered field selector is a
		// runtime error, not a compile one. Mirroring core's indexers here
		// keeps that dependency honest and tested.
		WithIndex(&karpv1.NodeClaim{}, "spec.nodeClassRef.group", func(o client.Object) []string {
			if ref := o.(*karpv1.NodeClaim).Spec.NodeClassRef; ref != nil {
				return []string{ref.Group}
			}
			return nil
		}).
		WithIndex(&karpv1.NodeClaim{}, "spec.nodeClassRef.kind", func(o client.Object) []string {
			if ref := o.(*karpv1.NodeClaim).Spec.NodeClassRef; ref != nil {
				return []string{ref.Kind}
			}
			return nil
		}).
		WithIndex(&karpv1.NodeClaim{}, "spec.nodeClassRef.name", func(o client.Object) []string {
			if ref := o.(*karpv1.NodeClaim).Spec.NodeClassRef; ref != nil {
				return []string{ref.Name}
			}
			return nil
		}).
		Build()
	return &NodeClassReconciler{Client: c, HostProvider: host.NewDefaultProvider(c, testNS)}, c
}

// claimFor builds a NodeClaim referencing a nodeclass of the given group/kind.
func claimFor(name, group, kind, className string) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: group, Kind: kind, Name: className},
		},
	}
}

// The termination finalizer is the only thing stopping a nodeclass from being
// deleted out from under NodeClaims that still need it to resolve hosts and
// credentials on their way out.
func TestFinalizeBlocksWhileNodeClaimsReference(t *testing.T) {
	nc := nodeClass("big", "prof")
	nc.Finalizers = []string{v1beta1.TerminationFinalizer}
	r, c := newNodeClassReconciler(t, nc,
		claimFor("claim-1", v1beta1.GroupVersion.Group, v1beta1.SSHNodeClassKind, "big"))

	if _, err := r.finalize(context.Background(), nc); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	got := &v1beta1.SSHNodeClass{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "big"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Finalizers) == 0 {
		t.Error("finalizer removed while a NodeClaim still references the nodeclass")
	}
}

func TestFinalizeReleasesWhenNoNodeClaimsReference(t *testing.T) {
	nc := nodeClass("big", "prof")
	nc.Finalizers = []string{v1beta1.TerminationFinalizer}
	r, c := newNodeClassReconciler(t, nc,
		// A claim for a DIFFERENT class, and one owned by another
		// cloudprovider entirely — neither may hold this class hostage.
		claimFor("other-class", v1beta1.GroupVersion.Group, v1beta1.SSHNodeClassKind, "small"),
		claimFor("foreign", "karpenter.k8s.aws", "EC2NodeClass", "big"))

	if _, err := r.finalize(context.Background(), nc); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	got := &v1beta1.SSHNodeClass{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "big"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Finalizers) != 0 {
		t.Errorf("finalizer still present with no referencing NodeClaims: %v", got.Finalizers)
	}
}

// The NodeClaim watch must ignore claims belonging to another cloudprovider —
// on a shared cluster an EC2NodeClass named "big" must not enqueue our "big".
func TestNodeClassForClaimIgnoresForeign(t *testing.T) {
	r, _ := newNodeClassReconciler(t)
	for _, tc := range []struct {
		name  string
		claim *karpv1.NodeClaim
		want  int
	}{
		{"ours", claimFor("c", v1beta1.GroupVersion.Group, v1beta1.SSHNodeClassKind, "big"), 1},
		{"foreign group", claimFor("c", "karpenter.k8s.aws", "EC2NodeClass", "big"), 0},
		{"our group wrong kind", claimFor("c", v1beta1.GroupVersion.Group, "SomethingElse", "big"), 0},
		{"no ref", &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "c"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(r.nodeClassForClaim(context.Background(), tc.claim)); got != tc.want {
				t.Errorf("enqueued %d requests, want %d", got, tc.want)
			}
		})
	}
}

func nodeClass(name, profile string) *v1beta1.SSHNodeClass {
	return &v1beta1.SSHNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1beta1.SSHNodeClassSpec{
			JoinProfileRef: corev1.LocalObjectReference{Name: profile},
		},
	}
}

func joinProfile(name string) *v1beta1.SSHJoinProfile {
	return &v1beta1.SSHJoinProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1beta1.SSHJoinProfileSpec{
			Scripts: v1beta1.ProfileScripts{Install: "i", Join: "j", Leave: "l"},
		},
	}
}

func reconcileNodeClass(t *testing.T, r *NodeClassReconciler, c client.Client, name string) *v1beta1.SSHNodeClass {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	nc := &v1beta1.SSHNodeClass{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, nc); err != nil {
		t.Fatal(err)
	}
	return nc
}

func TestNodeClassReadyConditions(t *testing.T) {
	t.Run("profile missing", func(t *testing.T) {
		r, c := newNodeClassReconciler(t, nodeClass("nc", "missing"))
		nc := reconcileNodeClass(t, r, c, "nc")
		cond := nc.StatusConditions().Get(status.ConditionReady)
		if !cond.IsFalse() || cond.Reason != "ProfileNotFound" {
			t.Fatalf("cond = %+v", cond)
		}
	})

	t.Run("no hosts", func(t *testing.T) {
		r, c := newNodeClassReconciler(t, nodeClass("nc", "p"), joinProfile("p"))
		nc := reconcileNodeClass(t, r, c, "nc")
		cond := nc.StatusConditions().Get(status.ConditionReady)
		if !cond.IsFalse() || cond.Reason != "NoHosts" {
			t.Fatalf("cond = %+v", cond)
		}
	})

	t.Run("ready", func(t *testing.T) {
		r, c := newNodeClassReconciler(t, nodeClass("nc", "p"), joinProfile("p"), probeHost("h", v1beta1.HostStateAvailable))
		nc := reconcileNodeClass(t, r, c, "nc")
		if cond := nc.StatusConditions().Get(status.ConditionReady); !cond.IsTrue() {
			t.Fatalf("cond = %+v", cond)
		}
	})
}

// TestNodeClassWatchMapping guards against readiness never re-evaluating:
// host and profile events must fan out to every nodeclass.
func TestNodeClassWatchMapping(t *testing.T) {
	r, _ := newNodeClassReconciler(t, nodeClass("a", "p"), nodeClass("b", "p"))
	reqs := r.allNodeClasses(context.Background(), probeHost("h", v1beta1.HostStateAvailable))
	if len(reqs) != 2 {
		t.Fatalf("want 2 requests, got %d", len(reqs))
	}
}
