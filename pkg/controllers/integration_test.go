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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/operator"
)

// TestIntegrationZombieGuard runs the hostprobe reconciler against a REAL
// apiserver (envtest) with a real informer cache — the layer where
// read-your-own-writes CAS failures live: after the reconciler patches probe
// facts, the informer does not yet contain that write, and a CAS based on a
// cache re-read would 409 deterministically, looping forever. Fake-client
// unit tests cannot catch that class; this test replays the e2e zombie-guard
// scenario (force-deleted NodeClaim → stale-claim release → zombie leave)
// end to end.
//
// Requires KUBEBUILDER_ASSETS (run via `make test-integration`).
func TestIntegrationZombieGuard(t *testing.T) {
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
	// Same index set the real operator registers — the probe resolves a host's
	// Node through it, and without it the List fails at runtime and the guard
	// silently wedges.
	if err := operator.RegisterFieldIndexes(ctx, mgr.GetFieldIndexer()); err != nil {
		t.Fatalf("field indexes: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &HostProbeReconciler{
		Client:              mgr.GetClient(),
		KubernetesInterface: clientset,
		Exec:                zombieExec(nil),
	}
	if err := reconciler.Register(ctx, mgr); err != nil {
		t.Fatalf("register: %v", err)
	}
	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("manager exited: %v", err)
		}
	}()

	// direct (uncached) client for fixture setup and assertions
	direct, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}

	if err := direct.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}); err != nil {
		t.Fatal(err)
	}
	if err := direct.Create(ctx, sshKeySecret()); err != nil {
		t.Fatal(err)
	}
	if err := direct.Create(ctx, joinProfile("prof")); err != nil {
		t.Fatal(err)
	}

	// zombie Node advertising the host's IP
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "zombie-node"}}
	if err := direct.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}}
	if err := direct.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}

	// host claimed by a NodeClaim that never existed (force-deleted upstream)
	host := probeHost("h", "")
	if err := direct.Create(ctx, host); err != nil {
		t.Fatal(err)
	}
	host.Status.State = v1beta1.HostStateInUse
	host.Status.ClaimRef = &v1beta1.ClaimReference{Name: "force-deleted"}
	host.Status.InstalledProfile = "prof@1"
	if err := direct.Status().Update(ctx, host); err != nil {
		t.Fatal(err)
	}

	// stale-claim release → probe → zombie fence → leave → node delete → Available
	err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		got := &v1beta1.SSHHost{}
		if err := direct.Get(ctx, types.NamespacedName{Namespace: testNS, Name: "h"}, got); err != nil {
			return false, err
		}
		if strings.Contains(got.Status.LastProbeError, "zombie leave failed") {
			return false, nil
		}
		return got.Status.State == v1beta1.HostStateAvailable && got.Status.ClaimRef == nil, nil
	})
	if err != nil {
		got := &v1beta1.SSHHost{}
		_ = direct.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "h"}, got)
		t.Fatalf("host never became Available (state=%s err=%q): %v — the zombie guard is stuck (CAS loop?)",
			got.Status.State, got.Status.LastProbeError, err)
	}

	if err := direct.Get(ctx, types.NamespacedName{Name: "zombie-node"}, &corev1.Node{}); !apierrors.IsNotFound(err) {
		t.Fatalf("zombie node must be deleted, got err=%v", err)
	}
}
