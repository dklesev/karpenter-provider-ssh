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
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// TestIntegrationStartupTaintGate runs the gate reconciler against a REAL
// apiserver with a real informer cache: the pod Ready flip arrives as a watch
// event through podReadyChanged, the optimistic-lock patch hits a Node the
// apiserver actually versions, and the resulting Node update must not
// re-trigger a second (conflicting) removal. Fake-client unit tests cover the
// decision table; this covers the wiring.
//
// Requires KUBEBUILDER_ASSETS (run via `make test-integration`).
func TestIntegrationStartupTaintGate(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set — run via 'make test-integration'")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd"),
			filepath.Join("..", "..", "config", "karpenter"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  clientgoscheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	// nodesForNodeClass lists NodeClaims through the nodeClassRef field
	// indexes karpenter core registers on the production manager.
	for _, f := range []string{"group", "kind", "name"} {
		field := f
		if err := mgr.GetFieldIndexer().IndexField(ctx, &karpv1.NodeClaim{}, "spec.nodeClassRef."+field, func(o client.Object) []string {
			ref := o.(*karpv1.NodeClaim).Spec.NodeClassRef
			if ref == nil {
				return nil
			}
			switch field {
			case "group":
				return []string{ref.Group}
			case "kind":
				return []string{ref.Kind}
			default:
				return []string{ref.Name}
			}
		}); err != nil {
			t.Fatalf("index %s: %v", field, err)
		}
	}
	if err := (&StartupTaintGateReconciler{Client: mgr.GetClient()}).Register(ctx, mgr); err != nil {
		t.Fatalf("register: %v", err)
	}
	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("manager exited: %v", err)
		}
	}()

	direct, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	// gateNS is kube-system, which envtest pre-creates.
	if err := direct.Create(ctx, gateNodeClass()); err != nil {
		t.Fatal(err)
	}
	claim := gateNodeClaim(gateKey)
	claim.UID = "" // the apiserver assigns it; the Node owner ref must use the real one
	claim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{{
		Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"},
	}}
	if err := direct.Create(ctx, claim); err != nil {
		t.Fatalf("nodeclaim: %v", err)
	}
	claim.Status.NodeName = gateNode
	if err := direct.Status().Update(ctx, claim); err != nil {
		t.Fatal(err)
	}
	node := taintedNode(gateKey)
	node.OwnerReferences[0].UID = claim.UID
	if err := direct.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	pod := agentPod("agent-a", gateNode, false)
	pod.Spec.Containers = []corev1.Container{{Name: "agent", Image: "agent:latest"}}
	if err := direct.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}

	// Not Ready yet: the gate must hold. Give the reconciler a moment to
	// observe the node, then assert the startup taint is still there.
	time.Sleep(2 * time.Second)
	got := &corev1.Node{}
	if err := direct.Get(ctx, types.NamespacedName{Name: gateNode}, got); err != nil {
		t.Fatal(err)
	}
	if !hasTaintKey(got.Spec.Taints, gateKey) {
		t.Fatalf("startup taint removed before the agent pod was Ready: %v", got.Spec.Taints)
	}

	// Ready flip → watch event → gate opens.
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := direct.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	err = wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := direct.Get(ctx, types.NamespacedName{Name: gateNode}, got); err != nil {
			return false, err
		}
		return !hasTaintKey(got.Spec.Taints, gateKey), nil
	})
	if err != nil {
		t.Fatalf("startup taint never removed after the agent pod became Ready: %v (taints=%v)", err, got.Spec.Taints)
	}
	if !hasTaintKey(got.Spec.Taints, permanentKY) {
		t.Fatalf("permanent taint must survive the gate patch: %v", got.Spec.Taints)
	}
}
