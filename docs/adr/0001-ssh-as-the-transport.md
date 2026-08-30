# 0001 — SSH is the transport; the shim is the API

**Status:** Accepted · **Date:** 2026-07-17

## Context

This provider makes *pre-existing* machines join and leave a cluster. It does
not create them. So the only thing it needs from a host is the ability to run a
few privileged operations on it — install, join, leave — and to read back some
facts.

The obvious alternative to SSH is an agent: install a small daemon on every pool
host, give it a typed API (`join`, `leave`, `probe`), and have the controller
call it. That is a real design, used widely, and the question of why this
project does not do it deserves a real answer rather than "SSH was easier".

The question usually arrives attached to a security argument: SSH means handing
a controller root on every host in the fleet, and that feels worse than a narrow
RPC surface.

## Decision

**SSH, with the shim (`kpssh-shim`) as the actual API.**

[Verified execution](../verified-exec.md) is not a hardening pass bolted onto
raw SSH — it *is* the agent design, with sshd doing the transport:

| what an agent gives you | how the shim gives it |
|---|---|
| a fixed operation vocabulary | `probe`/`install`/`join`/`leave`/`uninstall`, and nothing else is reachable |
| the caller cannot exceed the API | sshd `ForceCommand` pins the login to the shim; `sudo` is scoped to that one binary |
| typed inputs | `KPSSH_*` params, name-constrained by an allow-list |
| the caller cannot author code | scripts are signed offline; the controller holds no signing key |
| authentication | sshd, with a pinned host key and a source-locked `authorized_keys` |

The difference from a REST agent is that authentication, transport security, and
process isolation are delegated to sshd — 25 years of hardened, audited,
distro-patched code that is *already running and already being upgraded* on
every host — instead of being reimplemented as an HTTP server with mTLS, a
certificate rotation story, and a bootstrap problem for the certificate.

## Why not an agent

**It does not remove the daemon problem, it creates one.** The premise is a
fleet where SSH is all you get. If you can install and maintain an agent, you
can bake the shim in the same way — so the agent buys nothing on provisioning,
and costs a versioned, CVE-tracked root daemon listening on a new port on every
host, plus upgrade skew across a fleet you do not fully control.

**The profile model needs code to cross the boundary anyway.** The extensibility
story here is `SSHJoinProfile`: users bring their own join mechanism (kubeadm,
nodeadm, TLS bootstrap, nodeadm-ssm). An agent that accepts only *data* would
have to bake in every bootstrap variant, which kills "generic over any
TLS-bootstrap-capable control plane". An agent that accepts *scripts* is the
shim with extra steps. Signed-script-as-profile is what makes the generality
work, and signing is what makes it safe.

**The security argument does not survive contact with the actual bug.** The one
real vulnerability found here was a parameter-name escape: a param name reached
a bash process's environment, where `BASH_ENV` is expanded — including command
substitution — before a non-interactive script runs. An agent accepting
`{"params": {...}}` and exec'ing a script with them has the identical bug, from
the identical cause. It was "attacker-influenced names reach a shell's
environment", not "SSH is dangerous". The transport was incidental, and letting
that bug drive the architecture would have been fixing the wrong thing.

## Where an agent genuinely wins

Recorded honestly, because this is the case that would reopen the decision —
and it is **much narrower than "NAT'd fleets"**, which is where this argument
usually starts.

**NAT is not the problem, because the controller is not central.** It runs
in-cluster, and the chart's `nodeSelector`/`tolerations` place it on a node
*inside* the segment where its pool lives. From there the pool is on the LAN and
the NAT between that segment and the outside is irrelevant — no inbound path
from anywhere else is required. This is a deployed topology, not a theoretical
one, and [Installation](../installation.md#prerequisites) already states the
actual requirement: hosts must be SSH-reachable *from wherever the controller
runs*.

**The real constraint is one reachability domain per instance.** A single
controller must reach every host it manages. Segments that are mutually
unreachable from any one node would need one instance each — and two instances
cannot currently share a cluster: `GetSupportedNodeClasses` returns
`SSHNodeClass` for the kind, not for an instance, so karpenter core considers
*every* `SSHNodeClass` NodePool managed by *both*, and both provisioners would
answer the same pending pods. (Coexistence with a *foreign* karpenter works
precisely because the node class kinds differ — see
[Coexistence](../coexistence.md).) So the agent's advantage reduces to: fleets
where no cluster node can sit inside the segment at all, or many mutually
unreachable segments that would need sharding.

That is a real case, and if it becomes a target the cheaper fix is probably
instance-scoped NodePool ownership rather than a new transport.

Dial-out is also harder than it looks, and not for the reason people first
reach for:

- **Karpenter's push model is not the obstacle.** An agent holding a long-lived
  outbound stream is pushed "join now" at effectively zero latency (Teleport,
  Tailscale and GitHub Actions runners all work this way), and
  `CloudProvider.Create` is already permitted to block for minutes — EC2 does.
- **The obstacle is what it does to the controller.** It stops being a
  reconciler and becomes a stateful connection broker: an inbound-reachable
  endpoint, one open stream per host in the fleet, and leader election moved
  onto the data path — a failover drops every connection at once. A warm pool
  means idle hosts holding streams for hours to be useful for seconds.
- **The broker-free variant inverts the credential model.** Let the agent watch
  its own `SSHHost` through the API server and there is no broker — but now
  every idle host in the pool holds standing cluster credentials. One
  controller-held SSH key becomes N host-held credentials, each a foothold, all
  needing rotation. And the agent needs a credential to learn it should join the
  cluster that would have issued it: the same bootstrap problem, moved.

Neither model dominates. SSH concentrates the credential (one secret, N
targets — and verified exec bounds what a stolen one can do, since it cannot
sign). An agent distributes it (N secrets, one target). Which is worse depends
on whether you fear a controller compromise or a host compromise more.

## Consequences

- Every pool host must accept SSH **from wherever the controller runs** — which
  is a placement question, not a topology one. The constraint that actually
  bites is one reachability domain per controller instance (above), since a
  second instance in the same cluster would double-provision.
- The security of verified mode rests on sshd configuration (`ForceCommand`,
  `AcceptEnv`, `PermitUserEnvironment no`) — configuration the operator owns and
  can get wrong. Hence the provisioning recipe in
  [Verified execution](../verified-exec.md) being explicit rather than
  aspirational.
- `internal/sshexec` stays behind an interface. Not to abstract over a second
  transport speculatively, but so that adding one later is a new implementation
  rather than a rewrite.
- Raw mode (`sudo bash -s`, unsigned) still exists and is the default. It is the
  thing verified mode replaces, kept for the case where the operator already
  trusts everything that can write an `SSHJoinProfile`.

## Revisit if

- A fleet appears with segments that no single cluster node can reach — many
  small NAT'd sites, say — so sharding is unavoidable. Note the first thing to
  try then is instance-scoped NodePool ownership, not a transport rewrite: a
  controller placed inside each segment already works today, it just cannot
  currently share a cluster with a second one.
- A host population appears that genuinely cannot run sshd (some hardened
  images), rather than merely preferring not to.
- The signed-profile model proves too coarse — i.e. operators want the join
  logic versioned with the host rather than shipped from the cluster. That is
  the honest agent-shaped argument, and it is about *where code lives*, not
  about transport security.
