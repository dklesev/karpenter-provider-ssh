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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/controllers"
)

// Scenario budgets. Generous vs the warm-path reality (join is seconds warm on
// kindest/node) so a loaded parallel CI runner has headroom.
const (
	hostsReady  = 180 * time.Second
	joinReady   = 420 * time.Second
	consolidate = 420 * time.Second
	hostBack    = 300 * time.Second
	tryWindow   = 150 * time.Second // time to let the controller attempt a (failing) join
	holdReject  = 60 * time.Second  // invariant hold after a rejection settles
)

// rebootBack is the reboot scenario's own budget. Every other wait here races
// the controller, which reacts to a watch event; a rebooted host is genuinely
// unreachable, so the probe cadence sets the pace instead. A probe that lands
// while sshd is still coming up burns ProbeTimeout and then waits a full
// ProbeInterval, so each miss costs the sum. Budget three misses: two is
// exactly hostBack, which is why this flaked on a loaded runner and passed on
// an idle one. waitFor polls, so the wider bound costs nothing when it's warm.
var rebootBack = 3 * (controllers.ProbeInterval + controllers.ProbeTimeout)

// ---------------------------------------------------------------- Raw scenarios

// scale-up: a pending pod pulls a host into the cluster as a Ready node.
func TestE2EScaleUp(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 2)
	c.setupRawPool("30s")
	c.scaleSmoke(1)
	c.joinOneNode() // asserts a pool node Ready + smoke Running
}

// consolidation: an emptied node is drained and left; the host returns warm with
// kubelet AND containerd disabled — the reboot-safety precondition.
func TestE2EConsolidation(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 2)
	c.setupRawPool("30s")
	c.scaleSmoke(1)
	container := c.joinOneNode()

	c.scaleSmoke(0)
	c.waitFor(consolidate, "node consolidated away", c.noPoolNodes)
	c.waitFor(hostBack, "host Available again", func() bool { return c.countHostState(v1beta1.HostStateAvailable) == len(c.hosts) })
	c.assertUnitsDisabled(container)
}

// reboot: a restarted warm host must NOT rejoin (disabled units stay down).
func TestE2EReboot(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 2)
	c.setupRawPool("30s")
	c.scaleSmoke(1)
	container := c.joinOneNode()

	c.scaleSmoke(0)
	c.waitFor(consolidate, "node consolidated away", c.noPoolNodes)
	c.waitFor(hostBack, "host Available again", func() bool { return c.countHostState(v1beta1.HostStateAvailable) == len(c.hosts) })
	c.assertUnitsDisabled(container)

	c.logf("rebooting %s (docker restart == reboot: systemd re-inits, enabled units start)", container)
	mustExec(t.Context(), t, "docker", "restart", container)
	c.waitSSHDByName(container)
	c.holdFor(90*time.Second, "no pool node rejoined after reboot", c.noPoolNodes)
	c.waitFor(rebootBack, "rebooted host probes Available", func() bool { return c.countHostState(v1beta1.HostStateAvailable) == len(c.hosts) })
}

// zombie guard: a force-deleted NodeClaim (finalizers stripped, provider Delete
// never runs) leaves a running node behind; the probe must force-leave the host
// and delete the orphaned Node.
func TestE2EZombieGuard(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 2)
	// Slow consolidation right down so it cannot race the forced deletion — the
	// zombie path must be the one that cleans up.
	c.setupRawPool("10m")
	c.scaleSmoke(1)
	container := c.joinOneNode()
	zombieNode := c.poolNodeNames()[0]

	// Nothing should need rescheduling mid-scenario.
	c.scaleSmoke(0)
	nc := c.firstNodeClaim()
	if nc == "" {
		t.Fatal("no NodeClaim to force-delete")
	}
	c.logf("force-deleting %s (stripping finalizers — provider Delete never runs)", nc)
	c.kubectl("patch", nc, "--type", "merge", "-p", `{"metadata":{"finalizers":null}}`)
	_, _ = execRun(t.Context(), "kubectl", "--kubeconfig", c.kubeconfig, "delete", nc, "--wait=false")

	c.waitFor(joinReady, "zombie Node deleted by the guard", func() bool { return !c.nodeExists(zombieNode) })
	c.waitFor(hostBack, "host released to Available after zombie leave", func() bool { return c.countHostState(v1beta1.HostStateAvailable) == len(c.hosts) })
	if c.unitActive(container, "kubelet") {
		t.Fatalf("kubelet still active on %s after zombie leave", container)
	}
	c.holdFor(holdReject, "no node re-appears after zombie cleanup", c.noPoolNodes)
}

// power outage: a claimed host is powered off while attached (docker stop —
// filesystem persists, kubelet unit stays enabled) and powered back on later.
// Deleting the NodeClaim stands in for karpenter core's node repair, which
// force-terminates after tolerating NodeReady=False/Unknown for 10 minutes
// (RepairPolicies) — everything from the provider on is the real code path:
// Delete runs leave against a dead host, must fail, and must still release
// the claim, parking the host Unhealthy. Power-on then boots the still-enabled
// kubelet with warm credentials — the zombie rejoin — and the guard must
// leave + disable it and clean up without operator help.
func TestE2EPowerOutage(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 2)
	// Slow consolidation right down: smoke scales to 0 while the host is still
	// alive, and consolidation reaching the NodeClaim first would run a CLEAN
	// leave against a live host — the dead-host leave is the entire point.
	c.setupRawPool("10m")
	c.scaleSmoke(1)
	container := c.joinOneNode()

	// Empty the node while it is still reachable (see podsGone on why a pod
	// left behind would stall the drain).
	c.scaleSmoke(0)
	c.waitFor(hostBack, "smoke pod fully gone", func() bool { return c.podsGone("smoke") })

	c.logf("powering off %s (docker stop — claimed host goes dark)", container)
	mustExec(t.Context(), t, "docker", "stop", container)

	nc := c.firstNodeClaim()
	if nc == "" {
		t.Fatal("no NodeClaim to delete")
	}
	c.logf("deleting %s (stands in for node repair after the NotReady toleration)", nc)
	_, _ = execRun(t.Context(), "kubectl", "--kubeconfig", c.kubeconfig, "delete", nc, "--wait=false")

	// Leave cannot reach the powered-off host; the claim must release anyway.
	c.waitFor(hostBack, "host parked Unhealthy after dead-host leave", func() bool {
		return c.countHostState(v1beta1.HostStateUnhealthy) == 1
	})
	if got := c.claimedHostName(); got != "" {
		t.Fatalf("host %s still claimed after NodeClaim deletion", got)
	}

	c.logf("powering %s back on (enabled kubelet + warm credentials boot with it)", container)
	mustExec(t.Context(), t, "docker", "start", container)
	c.waitSSHDByName(container)

	// From here the zombie guard owns the cleanup: the next probe sees the
	// kubelet on an unclaimed host, force-runs leave, deletes any
	// re-registered Node. Probe cadence sets the pace — reboot budget.
	c.waitFor(rebootBack, "host probes Available after zombie cleanup", func() bool {
		return c.countHostState(v1beta1.HostStateAvailable) == len(c.hosts)
	})
	c.assertUnitsDisabled(container)
	c.holdFor(holdReject, "no zombie node lingers after cleanup", c.noPoolNodes)
}

// ----------------------------------------------------------- Verified scenarios

// verified join + leave: a signed profile joins through the ForceCommand shim
// (probe, install, join AND leave all verified end to end). trustedSigners is
// always set — the controller-side verify runs too (defence in depth).
func TestE2EVerifiedJoinAndLeave(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 1)
	c.setupVerifiedPool("30s", verifiedOpts{})
	c.scaleSmoke(1)
	container := c.joinOneNode() // Ready node == signed install+join ran via shim

	// Leave must also flow through the shim: scale down, host warm + units off.
	c.scaleSmoke(0)
	c.waitFor(consolidate, "verified node consolidated", c.noPoolNodes)
	c.waitFor(hostBack, "host Available after verified leave", func() bool { return c.countHostState(v1beta1.HostStateAvailable) == len(c.hosts) })
	c.assertUnitsDisabled(container)
}

// rogue signer rejected by the HOST shim: the join is validly signed by a key
// the CR's trustedSigners lists (so the controller-side check passes — the
// compromised-controller model, where whoever writes CRs extends that list at
// will), but the host's allowed_signers holds only the pool signer. install is
// signed by the pool signer and runs (installedProfile set — the verified
// pipeline works); the rogue-signed join must die at the shim, proving the
// host trust root stands alone.
func TestE2EVerifiedRogueSignerRejectedByShim(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 1)
	rogue := newSigner(t)
	c.setupVerifiedPool("30s", verifiedOpts{rogueJoin: rogue}, c.signer.pubLine, rogue.pubLine)
	c.scaleSmoke(1)

	// Positive control: the shim verified + ran the correctly-signed install.
	c.waitFor(tryWindow, "signed install ran through the shim (installedProfile set)", c.someHostInstalled)
	// Security invariant: the rogue-signed join never executes.
	c.holdFor(holdReject, "rogue-signed join never registers a node", c.noPoolNodes)
	if n := c.countHostState(v1beta1.HostStateInUse); n != 0 {
		t.Fatalf("host reached InUse (%d) despite a rogue-signed join", n)
	}
	if !c.sawRejection("signature rejected", "join phase", "exit status 2") {
		t.Log("warning: no explicit signature-rejection surfaced in logs/events (invariant still held)")
	}
}

// unsigned profile rejected by the CONTROLLER: execMode Verified with no
// signatures block. runVerifiedPhase refuses at the install phase BEFORE
// connecting — nothing ever runs on the host (installedProfile stays empty).
func TestE2EVerifiedUnsignedRejectedByController(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 1)
	c.setupVerifiedPool("30s", verifiedOpts{omitSignatures: true})
	c.scaleSmoke(1)

	// Positive control: karpenter did attempt a launch (a NodeClaim exists)...
	c.waitFor(tryWindow, "karpenter attempted a launch (NodeClaim created)", func() bool { return c.nodeClaimCount() > 0 })
	// ...but the controller refused pre-connect: nothing ran, no node registered.
	c.holdFor(holdReject, "unsigned verified profile never registers a node", c.noPoolNodes)
	if c.someHostInstalled() {
		t.Fatal("install ran despite a missing signature (controller gate failed)")
	}
	if !c.sawRejection("no signature for phase", "verified host") {
		t.Log("warning: no explicit controller refusal surfaced in logs/events (invariant still held)")
	}
}

// ---------------------------------------------------------------- setup helpers

// setupRawPool applies a Raw profile/nodeclass/nodepool/hosts and waits for all
// hosts to probe Available.
func (c *cluster) setupRawPool(consolidateAfter string) {
	c.t.Helper()
	c.applyProfileRaw()
	c.applyNodeClassAndPool(consolidateAfter)
	c.applyHosts(hostSpec{})
	c.createSmoke()
	c.waitFor(hostsReady, "all hosts Available", func() bool {
		return c.countHostState(v1beta1.HostStateAvailable) == len(c.hosts)
	})
}

// setupVerifiedPool is setupRawPool for Verified hosts — the probe here already
// exercises the ForceCommand shim (an unsigned, built-in probe phase).
// trustedSigners defaults to the pool signer; pass extra lines to model a CR
// that trusts more keys than the hosts do.
func (c *cluster) setupVerifiedPool(consolidateAfter string, opts verifiedOpts, trustedSigners ...string) {
	c.t.Helper()
	if len(trustedSigners) == 0 {
		trustedSigners = []string{c.signer.pubLine}
	}
	c.applyProfileVerified(opts)
	c.applyNodeClassAndPool(consolidateAfter)
	c.applyHosts(hostSpec{verified: true, trustedSigners: trustedSigners})
	c.createSmoke()
	c.waitFor(hostsReady, "all verified hosts Available (probed via shim)", func() bool {
		return c.countHostState(v1beta1.HostStateAvailable) == len(c.hosts)
	})
}

// joinOneNode waits for a pool node to be Ready and smoke Running, then returns
// the claimed host's container name.
func (c *cluster) joinOneNode() string {
	c.t.Helper()
	c.waitFor(joinReady, "pool node Ready + smoke Running", func() bool {
		return c.poolNodeReady() && c.podRunning("smoke")
	})
	name := c.claimedHostName()
	if name == "" {
		c.t.Fatal("no claimed host after join")
	}
	return c.containerFor(name)
}

// ---------------------------------------------------------------- assert helpers

func (c *cluster) assertUnitsDisabled(container string) {
	c.t.Helper()
	for _, unit := range []string{"kubelet", "containerd"} {
		if got := c.unitState(container, unit); got != "disabled" {
			c.t.Fatalf("unit %s on %s is %q, want disabled after leave", unit, container, got)
		}
	}
	c.logf("ok: kubelet + containerd disabled on %s", container)
}

func (c *cluster) someHostInstalled() bool {
	var l v1beta1.SSHHostList
	if err := c.cl.List(c.t.Context(), &l, client.InNamespace(c.ns)); err != nil {
		return false
	}
	for i := range l.Items {
		if l.Items[i].Status.InstalledProfile != "" {
			return true
		}
	}
	return false
}

func (c *cluster) nodeExists(name string) bool {
	var n corev1.Node
	return c.cl.Get(c.t.Context(), client.ObjectKey{Name: name}, &n) == nil
}

func (c *cluster) firstNodeClaim() string {
	out, _ := execOut(c.t.Context(), "kubectl", "--kubeconfig", c.kubeconfig, "get", "nodeclaims", "-o", "name")
	return firstLine(out)
}

func (c *cluster) nodeClaimCount() int {
	out, _ := execOut(c.t.Context(), "kubectl", "--kubeconfig", c.kubeconfig, "get", "nodeclaims", "-o", "name")
	out = strings.TrimSpace(out)
	if out == "" {
		return 0
	}
	return strings.Count(out, "\n") + 1
}

// sawRejection greps the controller log AND events for any of the substrings —
// evidence the failure was the intended rejection, not an unrelated fault.
func (c *cluster) sawRejection(subs ...string) bool {
	hay := c.controllerLogs(1000)
	if ev, err := execRun(c.t.Context(), "kubectl", "--kubeconfig", c.kubeconfig, "get", "events", "-A"); err == nil {
		hay += ev
	}
	for _, s := range subs {
		if strings.Contains(hay, s) {
			return true
		}
	}
	return false
}

func (c *cluster) unitState(container, unit string) string {
	out, _ := execRun(c.t.Context(), "docker", "exec", container, "systemctl", "is-enabled", unit)
	return strings.TrimSpace(out)
}

func (c *cluster) unitActive(container, unit string) bool {
	out, _ := execRun(c.t.Context(), "docker", "exec", container, "systemctl", "is-active", unit)
	return strings.TrimSpace(out) == "active"
}

// containerFor maps an SSHHost name (host-N) to its container (<cluster>-host-N).
func (c *cluster) containerFor(hostName string) string { return c.name + "-" + hostName }

// waitSSHDByName is waitSSHD for a bare container name (post-reboot).
func (c *cluster) waitSSHDByName(container string) { c.waitSSHD(&host{name: container}) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
