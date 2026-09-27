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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

const (
	gateKey     = "example.com/agent-not-ready"
	gateNS      = "kube-system"
	gateNode    = "pool-node"
	gateClaim   = "pool-claim"
	gateClass   = "pool"
	permanentKY = "dedicated"
)

var agentLabels = map[string]string{"app": "agent"}

// gateNodeClass returns a class with one podsReady gate on gateKey.
func gateNodeClass(gates ...v1beta1.StartupTaintGate) *v1beta1.SSHNodeClass {
	if gates == nil {
		gates = []v1beta1.StartupTaintGate{podsReadyGate(gateKey, 1)}
	}
	return &v1beta1.SSHNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: gateClass},
		Spec: v1beta1.SSHNodeClassSpec{
			JoinProfileRef:    corev1.LocalObjectReference{Name: "prof"},
			StartupTaintGates: gates,
		},
	}
}

func podsReadyGate(key string, minReady int32) v1beta1.StartupTaintGate {
	return v1beta1.StartupTaintGate{
		TaintKey: key,
		RemoveWhen: v1beta1.StartupTaintCondition{PodsReady: &v1beta1.PodsReadyCondition{
			Namespace: gateNS,
			Selector:  metav1.LabelSelector{MatchLabels: agentLabels},
			MinReady:  &minReady,
		}},
	}
}

// gateNodeClaim lists the given keys as startupTaints of the SSH class.
func gateNodeClaim(startupKeys ...string) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: gateClaim, UID: "claim-uid"},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: v1beta1.GroupVersion.Group, Kind: v1beta1.SSHNodeClassKind, Name: gateClass,
			},
		},
	}
	for _, k := range startupKeys {
		nc.Spec.StartupTaints = append(nc.Spec.StartupTaints, corev1.Taint{Key: k, Effect: corev1.TaintEffectNoExecute})
	}
	nc.Status.NodeName = gateNode
	return nc
}

// taintedNode is a registered pool node (NodeClaim owner ref, as core writes
// it) carrying the permanent `dedicated` taint plus the given startup keys.
func taintedNode(startupKeys ...string) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: gateNode,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: nodeClaimAPIVersion, Kind: "NodeClaim", Name: gateClaim, UID: "claim-uid",
			}},
		},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: permanentKY, Effect: corev1.TaintEffectNoSchedule}}},
	}
	for _, k := range startupKeys {
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: k, Effect: corev1.TaintEffectNoExecute})
	}
	return n
}

func agentPod(name, node string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gateNS, Labels: agentLabels},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

func newGateReconciler(t *testing.T, objs ...runtime.Object) (*StartupTaintGateReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithRuntimeObjects(objs...).
		Build()
	return &StartupTaintGateReconciler{Client: c}, c
}

func reconcileGateNode(t *testing.T, r *StartupTaintGateReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: gateNode}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func nodeTaintKeys(t *testing.T, c client.Client) []string {
	t.Helper()
	n := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: gateNode}, n); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(n.Spec.Taints))
	for _, tt := range n.Spec.Taints {
		keys = append(keys, tt.Key)
	}
	return keys
}

func assertTaints(t *testing.T, c client.Client, want ...string) {
	t.Helper()
	got := nodeTaintKeys(t, c)
	if len(got) != len(want) {
		t.Fatalf("taints = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("taints = %v, want %v", got, want)
		}
	}
}

func TestGateOpensWhenAgentPodReady(t *testing.T) {
	r, c := newGateReconciler(t, gateNodeClass(), gateNodeClaim(gateKey), taintedNode(gateKey),
		agentPod("agent-a", gateNode, true))

	res := reconcileGateNode(t, r)
	if res.RequeueAfter != 0 {
		t.Errorf("no gate pending, RequeueAfter = %v", res.RequeueAfter)
	}
	// permanent taint untouched, startup taint gone
	assertTaints(t, c, permanentKY)
}

func TestGateStaysClosedUntilReady(t *testing.T) {
	r, c := newGateReconciler(t, gateNodeClass(), gateNodeClaim(gateKey), taintedNode(gateKey),
		agentPod("agent-a", gateNode, false))

	res := reconcileGateNode(t, r)
	if res.RequeueAfter != gateRecheck {
		t.Errorf("pending gate must requeue after %v, got %v", gateRecheck, res.RequeueAfter)
	}
	assertTaints(t, c, permanentKY, gateKey)

	// pod becomes Ready → next reconcile opens the gate
	p := agentPod("agent-a", gateNode, true)
	if err := c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	reconcileGateNode(t, r)
	assertTaints(t, c, permanentKY)
}

func TestGateIgnoresReadyPodOnOtherNode(t *testing.T) {
	r, c := newGateReconciler(t, gateNodeClass(), gateNodeClaim(gateKey), taintedNode(gateKey),
		agentPod("agent-elsewhere", "other-node", true))

	reconcileGateNode(t, r)
	assertTaints(t, c, permanentKY, gateKey)
}

func TestGateNeverStripsTaintNotDeclaredAsStartupTaint(t *testing.T) {
	// The class gates on `dedicated`, but the NodeClaim lists it as a
	// permanent taint (spec.taints), not a startupTaint → must stay.
	r, c := newGateReconciler(t,
		gateNodeClass(podsReadyGate(permanentKY, 1)),
		gateNodeClaim( /* no startup taints */ ),
		taintedNode(),
		agentPod("agent-a", gateNode, true))

	reconcileGateNode(t, r)
	assertTaints(t, c, permanentKY)
}

func TestGateLeavesForeignStartupTaintAlone(t *testing.T) {
	// Two startup taints on the pool, only one gated here: the other is some
	// DaemonSet's own contract and must not be touched.
	const foreign = "other.example.com/not-ready"
	r, c := newGateReconciler(t, gateNodeClass(), gateNodeClaim(gateKey, foreign), taintedNode(gateKey, foreign),
		agentPod("agent-a", gateNode, true))

	reconcileGateNode(t, r)
	assertTaints(t, c, permanentKY, foreign)
}

func TestGateMinReadyCountsOnlyThisNode(t *testing.T) {
	r, c := newGateReconciler(t, gateNodeClass(podsReadyGate(gateKey, 2)), gateNodeClaim(gateKey), taintedNode(gateKey),
		agentPod("agent-a", gateNode, true),
		agentPod("agent-b", "other-node", true))

	reconcileGateNode(t, r)
	assertTaints(t, c, permanentKY, gateKey)

	if err := c.Create(context.Background(), agentPod("agent-c", gateNode, true)); err != nil {
		t.Fatal(err)
	}
	reconcileGateNode(t, r)
	assertTaints(t, c, permanentKY)
}

func TestGateOnNodeCondition(t *testing.T) {
	class := gateNodeClass(v1beta1.StartupTaintGate{
		TaintKey:   gateKey,
		RemoveWhen: v1beta1.StartupTaintCondition{NodeCondition: &v1beta1.NodeConditionCondition{Type: "NetworkUnavailable", Status: corev1.ConditionFalse}},
	})
	node := taintedNode(gateKey)
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeNetworkUnavailable, Status: corev1.ConditionTrue}}
	r, c := newGateReconciler(t, class, gateNodeClaim(gateKey), node)

	res := reconcileGateNode(t, r)
	if res.RequeueAfter != gateRecheck {
		t.Errorf("pending gate must requeue, got %v", res.RequeueAfter)
	}
	assertTaints(t, c, permanentKY, gateKey)

	node.Status.Conditions[0].Status = corev1.ConditionFalse
	if err := c.Status().Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	reconcileGateNode(t, r)
	assertTaints(t, c, permanentKY)
}

func TestGateNoOpWithoutNodeClaimOwner(t *testing.T) {
	// A hand-joined node with the same taint key: not karpenter's, not ours.
	node := taintedNode(gateKey)
	node.OwnerReferences = nil
	r, c := newGateReconciler(t, gateNodeClass(), node, agentPod("agent-a", gateNode, true))

	res := reconcileGateNode(t, r)
	if res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue %v", res.RequeueAfter)
	}
	assertTaints(t, c, permanentKY, gateKey)
}

func TestGateNoOpWhenNodeClassGone(t *testing.T) {
	r, c := newGateReconciler(t, gateNodeClaim(gateKey), taintedNode(gateKey), agentPod("agent-a", gateNode, true))

	reconcileGateNode(t, r)
	assertTaints(t, c, permanentKY, gateKey)
}

func TestGateSkipsForeignCloudProviderClaim(t *testing.T) {
	claim := gateNodeClaim(gateKey)
	claim.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: "karpenter.k8s.aws", Kind: "EC2NodeClass", Name: gateClass}
	r, c := newGateReconciler(t, gateNodeClass(), claim, taintedNode(gateKey), agentPod("agent-a", gateNode, true))

	reconcileGateNode(t, r)
	assertTaints(t, c, permanentKY, gateKey)
}

func TestPodReadyChangedPredicate(t *testing.T) {
	p := podReadyChanged()
	old := agentPod("a", gateNode, false)
	if !p.Update(updateEvent(old, agentPod("a", gateNode, true))) {
		t.Error("Ready flip must pass")
	}
	if p.Update(updateEvent(old, agentPod("a", gateNode, false))) {
		t.Error("unchanged readiness must not pass")
	}
	if !p.Create(createEvent(old)) {
		t.Error("creates pass (default)")
	}
}

func updateEvent(oldObj, newObj client.Object) event.UpdateEvent {
	return event.UpdateEvent{ObjectOld: oldObj, ObjectNew: newObj}
}

func createEvent(obj client.Object) event.CreateEvent { return event.CreateEvent{Object: obj} }

func TestNodesForNodeClassMapsClaimNodeNames(t *testing.T) {
	// nodeclaimutils.ForNodeClass lists through core's nodeClassRef indexes —
	// mirrored here as in nodeclass_test.go.
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithRuntimeObjects(gateNodeClaim(gateKey)).
		WithIndex(&karpv1.NodeClaim{}, "spec.nodeClassRef.group", func(o client.Object) []string {
			return []string{o.(*karpv1.NodeClaim).Spec.NodeClassRef.Group}
		}).
		WithIndex(&karpv1.NodeClaim{}, "spec.nodeClassRef.kind", func(o client.Object) []string {
			return []string{o.(*karpv1.NodeClaim).Spec.NodeClassRef.Kind}
		}).
		WithIndex(&karpv1.NodeClaim{}, "spec.nodeClassRef.name", func(o client.Object) []string {
			return []string{o.(*karpv1.NodeClaim).Spec.NodeClassRef.Name}
		}).
		Build()
	r := &StartupTaintGateReconciler{Client: c}

	reqs := r.nodesForNodeClass(context.Background(), gateNodeClass())
	if len(reqs) != 1 || reqs[0].Name != gateNode {
		t.Fatalf("requests = %v, want [%s]", reqs, gateNode)
	}
	classNoGates := gateNodeClass()
	classNoGates.Spec.StartupTaintGates = nil
	if reqs := r.nodesForNodeClass(context.Background(), classNoGates); len(reqs) != 0 {
		t.Fatalf("class without gates must not fan out, got %v", reqs)
	}
}
