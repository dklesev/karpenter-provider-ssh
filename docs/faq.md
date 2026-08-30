# FAQ

## Why not cluster-autoscaler on static nodes?

Cluster-autoscaler scales *node groups it can create members of*. Point it at a
fixed set of machines and there is nothing for it to do: it has no API to add
the 11th node, because the 11th machine already exists and is simply not a
member. This project scales **membership** instead of machines — the pending pod
does not cause a machine to be built, it causes an existing machine to join.

## Why not Cluster API, MAAS, Tinkerbell, or a cloud provider?

Use them if you can. They **provision machines**, which is strictly better than
what this does: provisioned capacity is elastic, this is a fixed pool that can
run out. This project exists for the case where none of that is available —
someone hands you a rack, or an EC2 fleet you do not control, and the only
interface you get is SSH. If your infrastructure can create machines on demand,
that is the right path and this is the wrong tool
([ROADMAP](https://github.com/dklesev/karpenter-provider-ssh/blob/main/ROADMAP.md) — machine
provisioning is explicitly *not planned*).

## Does this only work with EKS?

No. `tls-bootstrap` works against any conformant control plane — verified on
kind, k0s and Exoscale SKS. EKS Hybrid Nodes is the *flagship* case rather than
the only one, because there attachment itself is billed per vCPU-hour, so
membership maps 1:1 onto the invoice and the savings are measurable rather than
theoretical. On a self-managed control plane the same machinery buys you
bin-packing and a warm pool, not a smaller bill.

## Do I need a separate karpenter installation?

No. Karpenter core is compiled into this controller — one Deployment. If you
*already* run a karpenter (say the AWS provider), the two coexist: this one
never touches `karpenter.sh` CRDs it does not own and ignores NodePools bound to
another cloudprovider. See [coexistence](coexistence.md).

## Can it power hosts on and off?

No — a host must be reachable over SSH for the provider to do anything with it,
so an off machine is invisible to it. Wake-on-LAN / BMC / IPMI power control is
[not planned](https://github.com/dklesev/karpenter-provider-ssh/blob/main/ROADMAP.md):
if you have a BMC you have a provisioning API, and a Cluster API provider will
serve you better. Power that is *not* a provisioning API — a PoE switch port, a
smart plug, a Pi's RTC alarm — composes with the provider from the outside;
[SBC fleets](sbc.md#power-is-not-this-providers-job) draws the line and gives
the hand-off protocol.

## What happens when the pool runs out?

The NodeClaim gets an `InsufficientCapacity` error, exactly as a cloud provider
would return when a instance type is unavailable, and karpenter reschedules or
leaves the pod pending. A fixed pool has a hard ceiling; that is the trade.

## Is my join script really run as root over SSH?

In the default (`Raw`) mode, yes — and it is worth being blunt about the
consequence: anyone who can write an `SSHJoinProfile` (a cluster-scoped CRD) can
run arbitrary root commands on every host in the pool. Lock that CRD down with
RBAC. If that model does not suit you, [verified execution](verified-exec.md) is
the opt-in answer: scripts signed by an offline key, verified by the controller
and re-verified on the host by a shim pinned via `ForceCommand`.

## Why SSH? Wouldn't an agent on each host be safer?

The agent you are picturing is what verified mode already is. The shim has a
fixed operation vocabulary (`probe`/`install`/`join`/`leave`/`uninstall`),
sshd's `ForceCommand` makes it the *only* thing the login can invoke, `sudo` is
scoped to that one binary, params are a typed allow-list, and the controller
holds no signing key so it cannot author code. That is a narrow RPC surface —
it just borrows sshd for transport and authentication instead of shipping an
HTTP server, mTLS, and a certificate-bootstrap problem, and instead of adding a
root daemon with its own CVE feed to every host in the fleet.

Worth knowing: the one real vulnerability found here (a parameter name reaching
a bash process's environment, where `BASH_ENV` expands before the script runs)
was transport-independent. An agent taking `{"params": {...}}` would have had
the identical bug.

The honest limit is reachability, not safety — and it is narrower than it
sounds. The controller runs in-cluster, so a NAT'd fleet is handled by *placing*
it on a node inside that segment (`nodeSelector`/`tolerations`): the pool is
then on the LAN and no inbound path from outside is needed. What actually
constrains you is that one instance must reach every host it manages, and two
instances cannot yet share a cluster. See
[ADR 0001](adr/0001-ssh-as-the-transport.md).

## Does it support DRA (Dynamic Resource Allocation)?

Not yet. Karpenter v1.14's own DRA support is experimental and **off by
default** (`--ignore-dra-requests`, default `true` — compiled into this
controller like every other core flag): with the default, a pod requesting
devices through ResourceClaims is skipped by the provisioner and never
triggers a launch. This provider adds no DRA device modeling on top —
instance types carry no `DynamicResources` templates, and the chart grants no
`resource.k8s.io` RBAC — so the flag is not usable here as shipped.

What **does** work today is the classic device-plugin path: declare extended
resources (`nvidia.com/gpu: 2`) in `SSHHost.spec.capacity`, install the
device plugin via the join profile, and select them with ordinary
`resources.requests` — karpenter bin-packs and launches on them like any
other resource. Modeling DRA for a fixed pool is on the
[roadmap](https://github.com/dklesev/karpenter-provider-ssh/blob/main/ROADMAP.md).

## Why is `topology.kubernetes.io/zone` always `pool`?

A hybrid pool has no cloud zones, but karpenter's topology machinery wants a
value. It is a stable literal — put a real zone name in a NodePool requirement
and it will match nothing ([api.md](api.md#well-known-labels-the-provider-advertises)).

## Why does a host class advertise the *smallest* capacity of its hosts?

Because an instance type is a promise: karpenter bin-packs pods against it
before it knows which host it will get. Advertising the largest host would let
it promise capacity a smaller sibling cannot honour, and the pods would not fit
after joining. Split uneven machines into separate classes — one class per
homogeneous group. Making a heterogeneous class stop collapsing to its smallest
member is on the
[roadmap](https://github.com/dklesev/karpenter-provider-ssh/blob/main/ROADMAP.md).

## Is the warm pool actually faster?

Yes, and it is the point. `install` runs once per host and is cached
(`status.installedProfile`); every later join is a kubelet registration —
seconds. On EKS the SSM registration and node identity survive a leave, so a
rejoin consumes no activation and keeps the same `mi-*` identity.

## Does deleting the Node really stop EKS Hybrid billing?

Billing follows the **Node object**, not whether kubelet is running. That is why
`leave` stops kubelet *and* the provider (or, for a zombie, the probe) deletes
the Node: stopping kubelet alone leaves the Node object behind and the meter
running. See [concepts](concepts.md).
