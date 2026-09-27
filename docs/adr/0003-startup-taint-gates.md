# 0003 — Startup ordering is a node-class gate, not a registration hook

**Status:** Accepted · **Date:** 2026-09-22

## Context

Pool hosts rejoin warm in seconds. That speed exposes a race cloud nodes
rarely show: a pod that needs something from a node-local DaemonSet at
startup — cloud credentials from an agent, a CNI datapath — is scheduled and
started before that DaemonSet's pod is Ready, crashes, and depending on how it
caches the failure may not recover on restart.

Kubernetes' answer is a **startup taint**: the node registers tainted, the
DaemonSets that must run first tolerate it, and the taint comes off when they
are up. Karpenter models this as NodePool `startupTaints` (stamped onto the
Node at registration, ignored for provisioning decisions, and a precondition
for `Initialized`). What karpenter does *not* model is **who removes the
taint**. The convention is that the tolerating DaemonSet removes its own taint
(cilium's `node.cilium.io/agent-not-ready`). Managed addons and third-party
agents cannot be taught that, so on their own the startup taints would stay
forever.

Three places could hold the removal logic.

## Options

**1. Let the DaemonSet remove its own taint.** Correct where the component
supports it; unavailable for exactly the components that motivated this —
managed addons whose manifests the operator does not control. Rejected as the
only mechanism, remains the preferred one where it exists.

**2. A karpenter registration hook.** Core exposes
`cloudprovider.NodeLifecycleHook`: a provider can hold registration — and
with it the `karpenter.sh/unregistered:NoExecute` taint — until a precondition
holds. Smallest code, no new taint, no NodePool change. Rejected because it
overloads *Registered* ("core has synced labels/taints onto the Node") with
"the node's daemons are up", and because it inherits core's fixed 15-minute
registration timeout: a broken agent would not leave a visibly stuck node, it
would delete the NodeClaim, leave the host, and rejoin it in a loop. Startup
ordering is a property of the node's *initialization*, and core already has
the right condition for it (`Initialized=Unknown`, `StartupTaintsExist`).

**3. A gate controller driven by the node class.** The NodePool declares the
startup taint (that is core's API and stays untouched); the `SSHNodeClass`
declares the observable condition under which it comes off. A provider
controller watches tainted pool nodes and the pods on them and patches the
taint away when the condition holds.

## Decision

Option 3: `SSHNodeClass.spec.startupTaintGates[]`, each a `taintKey` plus a
`removeWhen` union (`podsReady {namespace, selector, minReady}` or
`nodeCondition {type, status}`), enforced by a `startuptaintgate` reconciler.

Invariants the implementation must keep:

- A gate removes a taint only if the key is listed in the owning NodeClaim's
  `spec.startupTaints`. The class can never strip a permanent taint, a
  kubelet-registered taint, or a controller's taint by naming the same key.
- Only nodes whose NodeClaim references an `SSHNodeClass` are touched;
  another cloudprovider's nodes on a shared cluster and hand-joined nodes are
  invisible to it.
- `removeWhen` is a strict union (CEL: exactly one member). New condition
  kinds are added as new members, never by overloading an existing one with
  mode flags.
- The gate is not part of the node class `JoinHash`: editing it never rolls
  nodes.
- No provider-side timeout. A gate that never opens leaves the NodeClaim at
  `Initialized=Unknown/StartupTaintsExist`, which is the honest state.

## Cost

Two objects must agree on a key (NodePool `startupTaints` and node class
`startupTaintGates`); a mismatch is a silent no-op, surfaced only by the
`Initialized` condition and a debug log line. The controller adds a Node and
Pod watch — both informers already exist for karpenter core, so the cache cost
is nil, but tainted pool nodes now reconcile on every status heartbeat (cache
reads only).

## What would overturn it

Karpenter core growing a first-class "startup taint owner" concept — a
declarative removal condition on the NodePool itself — would make this
provider-side gate redundant and it should then be retired in favour of it.
