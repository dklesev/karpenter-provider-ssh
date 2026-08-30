//go:build e2e

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

package e2e

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

const nodePoolName = "e2e-pool"

// waitFor polls cond every 5s until it returns true or the timeout elapses
// (fatal). The typed reads inside cond swallow transient errors as "not yet".
func (c *cluster) waitFor(timeout time.Duration, desc string, cond func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			c.logf("ok: %s", desc)
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out after %s waiting for: %s", timeout, desc)
		}
		time.Sleep(5 * time.Second)
	}
}

// holdFor asserts cond stays true for the whole duration (fatal on first
// violation) — used for "must NOT happen" invariants like a warm host not
// rejoining.
func (c *cluster) holdFor(dur time.Duration, desc string, cond func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		if !cond() {
			c.t.Fatalf("violated during hold: %s", desc)
		}
		time.Sleep(5 * time.Second)
	}
	c.logf("held %s: %s", dur, desc)
}

// --------------------------------------------------------- typed cluster reads

func (c *cluster) countHostState(state v1beta1.HostState) int {
	var l v1beta1.SSHHostList
	if err := c.cl.List(c.t.Context(), &l, client.InNamespace(c.ns)); err != nil {
		return -1
	}
	n := 0
	for i := range l.Items {
		if l.Items[i].Status.State == state {
			n++
		}
	}
	return n
}

// claimedHostName returns the name of the (single) host currently carrying a
// claimRef, or "" if none.
func (c *cluster) claimedHostName() string {
	var l v1beta1.SSHHostList
	if err := c.cl.List(c.t.Context(), &l, client.InNamespace(c.ns)); err != nil {
		return ""
	}
	for i := range l.Items {
		if l.Items[i].Status.ClaimRef != nil {
			return l.Items[i].Name
		}
	}
	return ""
}

func (c *cluster) poolNodeNames() []string {
	var l corev1.NodeList
	if err := c.cl.List(c.t.Context(), &l, client.MatchingLabels{"karpenter.sh/nodepool": nodePoolName}); err != nil {
		return nil
	}
	names := make([]string, 0, len(l.Items))
	for i := range l.Items {
		names = append(names, l.Items[i].Name)
	}
	return names
}

func (c *cluster) noPoolNodes() bool { return len(c.poolNodeNames()) == 0 }

// poolNodeReady reports whether at least one pool node exists and is Ready.
func (c *cluster) poolNodeReady() bool {
	var l corev1.NodeList
	if err := c.cl.List(c.t.Context(), &l, client.MatchingLabels{"karpenter.sh/nodepool": nodePoolName}); err != nil {
		return false
	}
	for i := range l.Items {
		if nodeReady(&l.Items[i]) {
			return true
		}
	}
	return false
}

func (c *cluster) podRunning(app string) bool {
	var l corev1.PodList
	if err := c.cl.List(c.t.Context(), &l, client.MatchingLabels{"app": app}); err != nil {
		return false
	}
	return len(l.Items) > 0 && l.Items[0].Status.Phase == corev1.PodRunning
}

// podsGone reports that no pod with the app label exists at all — Terminating
// still counts as present. The power-outage scenario needs this before cutting
// power: a pod stuck Terminating on a dead kubelet never confirms deletion,
// which would stall karpenter's drain and with it the provider Delete under
// test.
func (c *cluster) podsGone(app string) bool {
	var l corev1.PodList
	if err := c.cl.List(c.t.Context(), &l, client.MatchingLabels{"app": app}); err != nil {
		return false
	}
	return len(l.Items) == 0
}

func nodeReady(n *corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
