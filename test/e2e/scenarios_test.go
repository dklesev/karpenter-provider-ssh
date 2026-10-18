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
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

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
	// drain + systemd shutdown (~25s on kindest/node) + boot + kubelet Ready +
	// core's 15s reboot poll.
	repairReboot = 420 * time.Second
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

// repair reboot: the reboot node repair commits for a NotReady node restarts
// the claimed host in place through the profile's reboot phase — same Node,
// same NodeClaim, new boot. Committed directly, so the scenario needs neither
// the NodeRepair gate nor its 10-minute toleration.
func TestE2ERepairReboot(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 1)
	c.setupRawPool("10m") // consolidation must not race the reboot
	c.scaleSmoke(1)
	c.rebootInPlace(c.joinOneNode())
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

// verified repair reboot: as TestE2ERepairReboot, with the signed reboot phase
// going through the ForceCommand shim.
func TestE2EVerifiedRepairReboot(t *testing.T) {
	t.Parallel()
	c := newCluster(t, 1)
	c.setupVerifiedPool("10m", verifiedOpts{})
	c.scaleSmoke(1)
	c.rebootInPlace(c.joinOneNode())
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

// rebootInPlace commits a reboot on the pool's only NodeClaim and asserts it
// ends as a reboot, not a replacement: core reports RebootSucceeded, the host
// container really restarted, the Node's bootID changed, and the same
// NodeClaim still holds the host with its workload back.
func (c *cluster) rebootInPlace(container string) {
	c.t.Helper()
	nc := c.onlyNodeClaim()
	node := nc.Status.NodeName
	before := c.nodeBootID(node)

	// Pool hosts run without a restart policy. A rebooting container exits
	// (systemd's in-container reboot, exit 129); this brings it back the way a
	// rebooted machine comes back.
	mustExec(c.t.Context(), c.t, "docker", "update", "--restart=on-failure", container)
	c.commitReboot(nc.Name)

	c.waitFor(repairReboot, "reboot succeeded", func() bool {
		reason, msg := c.rebootReason(nc.Name)
		if reason == karpv1.RebootReasonFailed {
			c.t.Fatalf("reboot failed: %s", msg)
		}
		return reason == karpv1.RebootReasonSucceeded
	})
	if got := c.restartCount(container); got != "1" {
		c.t.Fatalf("host container restarted %s times, want 1 (the reboot)", got)
	}
	if after := c.nodeBootID(node); after == before {
		c.t.Fatalf("Node %s bootID unchanged (%s) after the reboot", node, after)
	}
	if got := c.onlyNodeClaim(); got.UID != nc.UID {
		c.t.Fatalf("NodeClaim replaced (%s -> %s): want a reboot in place", nc.Name, got.Name)
	}
	if n := c.countHostState(v1beta1.HostStateInUse); n != 1 {
		c.t.Fatalf("%d hosts InUse after the reboot, want the claim kept", n)
	}
	c.waitFor(joinReady, "smoke Running again on the rebooted node", func() bool { return c.podRunning("smoke") })
}

// commitReboot writes what node repair writes when it picks RebootNode: the
// reboot drain bound (0s, forceful, like our RepairPolicies) and
// Rebooting=True/RebootRequested. Core's reboot controller does the rest.
func (c *cluster) commitReboot(name string) {
	c.t.Helper()
	ctx := c.t.Context()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		nc := &karpv1.NodeClaim{}
		if err := c.cl.Get(ctx, client.ObjectKey{Name: name}, nc); err != nil {
			return err
		}
		stored := nc.DeepCopy()
		if nc.Annotations == nil {
			nc.Annotations = map[string]string{}
		}
		nc.Annotations[karpv1.RebootTerminationGracePeriodAnnotationKey] = "0s"
		if err := c.cl.Patch(ctx, nc, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		stored = nc.DeepCopy()
		nc.StatusConditions().SetTrueWithReason(karpv1.ConditionTypeRebooting, karpv1.RebootReasonRequested, "committed by e2e")
		return c.cl.Status().Patch(ctx, nc, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		c.t.Fatalf("commit reboot on NodeClaim %s: %v", name, err)
	}
	c.logf("committed a reboot on NodeClaim %s", name)
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

// onlyNodeClaim returns the single NodeClaim the reboot scenarios run with.
func (c *cluster) onlyNodeClaim() *karpv1.NodeClaim {
	c.t.Helper()
	var l karpv1.NodeClaimList
	if err := c.cl.List(c.t.Context(), &l); err != nil {
		c.t.Fatalf("list NodeClaims: %v", err)
	}
	if len(l.Items) != 1 {
		c.t.Fatalf("%d NodeClaims, want exactly 1", len(l.Items))
	}
	return &l.Items[0]
}

// rebootReason is the NodeClaim's Rebooting condition reason and message ("" if
// unset or unreadable — waitFor treats that as "not yet").
func (c *cluster) rebootReason(name string) (string, string) {
	nc := &karpv1.NodeClaim{}
	if err := c.cl.Get(c.t.Context(), client.ObjectKey{Name: name}, nc); err != nil {
		return "", ""
	}
	cond := nc.StatusConditions().Get(karpv1.ConditionTypeRebooting)
	if cond == nil {
		return "", ""
	}
	return cond.Reason, cond.Message
}

func (c *cluster) nodeBootID(name string) string {
	c.t.Helper()
	var n corev1.Node
	if err := c.cl.Get(c.t.Context(), client.ObjectKey{Name: name}, &n); err != nil {
		c.t.Fatalf("get Node %s: %v", name, err)
	}
	return n.Status.NodeInfo.BootID
}

func (c *cluster) restartCount(container string) string {
	c.t.Helper()
	return strings.TrimSpace(mustExec(c.t.Context(), c.t, "docker", "inspect", "-f", "{{.RestartCount}}", container))
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
