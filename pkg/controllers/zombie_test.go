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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dklesev/karpenter-provider-ssh/internal/sshexec"
	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

// zombieExec answers the probe script with an active kubelet and lets the
// leave script succeed.
func zombieExec(leaveErr error) sshexec.Runner {
	return func(_ context.Context, _ sshexec.Target, script string, _ map[string]string) (*sshexec.Result, error) {
		if strings.HasPrefix(script, "nproc") {
			return &sshexec.Result{Stdout: "4\n16384000\nx86_64\nactive\n", HostKeyFingerprint: "SHA256:probed"}, nil
		}
		return &sshexec.Result{}, leaveErr
	}
}

// TestZombieAfterStaleClaimRelease replays the e2e zombie-guard scenario:
// NodeClaim force-deleted → stale-claim release → probe sees active kubelet →
// guard leaves the host and deletes the zombie Node → host Available again.
func TestZombieAfterStaleClaimRelease(t *testing.T) {
	h := probeHost("h", v1beta1.HostStateInUse)
	h.Status.ClaimRef = &v1beta1.ClaimReference{Name: "gone"}
	h.Status.InstalledProfile = "prof@1"

	zombieNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "zombie-node"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
		}},
	}

	r, c := newReconciler(t, zombieExec(nil), h, joinProfile("prof"), zombieNode)

	// reconcile 1: stale claim released → Pending
	reconcileHost(t, r, "h")
	got := getHost(t, c, "h")
	if got.Status.State != v1beta1.HostStatePending || got.Status.ClaimRef != nil {
		t.Fatalf("after release: state=%s claimRef=%v", got.Status.State, got.Status.ClaimRef)
	}

	// reconcile 2: probe → zombie guard → leave → node deleted → Available
	reconcileHost(t, r, "h")
	got = getHost(t, c, "h")
	if got.Status.State != v1beta1.HostStateAvailable {
		t.Fatalf("state=%s lastProbeErr=%q, want Available", got.Status.State, got.Status.LastProbeError)
	}
	if got.Status.InstalledProfile != "prof@1" {
		t.Fatalf("install cache must survive zombie leave, got %q", got.Status.InstalledProfile)
	}
	node := &corev1.Node{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "zombie-node"}, node); !apierrorsIsNotFound(err) {
		t.Fatalf("zombie node must be deleted, got err=%v", err)
	}
}

// TestZombieLeaveFailureParksUnhealthy: the guard fences the host, leave
// fails → parked Unhealthy with the error surfaced, Node kept (kubelet would
// re-create it anyway).
func TestZombieLeaveFailureParksUnhealthy(t *testing.T) {
	h := probeHost("h", v1beta1.HostStatePending)
	h.Status.InstalledProfile = "prof@1"

	r, c := newReconciler(t, zombieExec(context.DeadlineExceeded), h, joinProfile("prof"))

	reconcileHost(t, r, "h")
	got := getHost(t, c, "h")
	if got.Status.State != v1beta1.HostStateUnhealthy {
		t.Fatalf("state=%s, want Unhealthy", got.Status.State)
	}
	if !strings.Contains(got.Status.LastProbeError, "zombie leave failed") {
		t.Fatalf("lastProbeError=%q", got.Status.LastProbeError)
	}
}

func apierrorsIsNotFound(err error) bool {
	return client.IgnoreNotFound(err) == nil && err != nil
}
