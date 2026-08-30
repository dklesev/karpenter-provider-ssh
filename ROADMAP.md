# Roadmap

No dates, no promises. This is what the project knows it cannot do yet, and the
order it would fix things in. Priorities follow demand — say so in a
[Discussion](https://github.com/dklesev/karpenter-provider-ssh/discussions) if
one of these blocks you.

## Where it stands

`v1beta1`. Verified end to end on four control planes (kind, k0s, Exoscale SKS,
EKS Hybrid Nodes on a live cluster) and a Raspberry Pi 4 edge pool. Breaking
API changes bump the minor version and are called out in the release notes
with migration steps, until a battle-tested `v1`.

## Known limits (v1beta1)

| limit | today | why it is like this |
|---|---|---|
| **capacity per host class** | a class advertises the per-resource **minimum** across its hosts; a resource only some hosts have (a GPU) is dropped from the class | any host of a class can answer a claim, so karpenter must bin-pack against what the smallest member can deliver. Mixed classes silently shrink — keep hosts uniform per class |
| **architecture per host class** | a class whose hosts differ in arch is skipped entirely — never advertised, never claimable (error in the log) | one advertised arch for a class where half the hosts cannot run the image is worse than no class at all. Workaround: per-arch classes (`bm-amd64`, `bm-arm64`) |
| **`leave` is context-free** | leave scripts get no `.Vars`/`.Secrets`, only the host addresses | the release path and the zombie guard run leave without a node class in hand ([contract](docs/join-profiles.md#the-leave-contract-empty-render-context)). Parameterized leaves would need the join context persisted on the host |
| **one pool namespace** | all SSHHosts and their Secrets live in `POOL_NAMESPACE` | keeps the Secret RBAC to a single namespaced Role |
| **`Adopt` matches on InternalIP** | the provider finds the joined Node by matching `spec.nodeAddress`/`address` against Node InternalIPs | works for nodeadm/EKS; a host whose node registers a different IP needs `spec.nodeAddress` set explicitly |
| **no power management** | hosts must be powered on and SSH-reachable while in the pool | the pool is warm by design: membership is the scaling unit, not the machine. BMC/IPMI power control is a different provider |
| **validation is CEL-only** | no admission webhook | no webhook to operate, no cert rotation, no failure mode where the webhook takes the cluster down. The cost is that some invariants (e.g. a valid `leave` template) are reported on the node class's `Ready` condition instead of rejected at write time |
| **DRA requests are not modeled** | pods requesting devices via ResourceClaims never trigger a launch (karpenter's `--ignore-dra-requests` defaults to `true`); device-plugin extended resources in `spec.capacity` work normally | karpenter v1.14's DRA support is explicitly pre-GA (the flag exists only until it is), and modeling it for a fixed pool needs per-class device declarations — API surface worth adding once upstream settles |

## Candidates

Roughly in the order they would be worth doing:

- **Instance-scoped NodePool ownership (sharding).** Today one instance per
  cluster: `GetSupportedNodeClasses` claims the `SSHNodeClass` *kind*, so two
  instances would both answer every pending pod. A pool living behind NAT is
  already fine — schedule the controller onto a node inside that segment — but a
  cluster with pools in mutually unreachable segments needs one instance each,
  and that needs ownership scoped per instance rather than per kind. See
  [ADR 0001](docs/adr/0001-ssh-as-the-transport.md).

- **Per-host capacity within a class** — model each host as its own offering so
  a heterogeneous class stops collapsing to its smallest member.
- **Automatic arch splitting** — derive the effective class from
  `host-class` + `observedArch` instead of skipping mixed classes.
- **A `v1` API** once `v1beta1` has been battle-tested beyond its author; that
  promotion is the moment for any remaining naming cleanup, in one migration.
- **Scheduled maintenance windows** — the `maintenance` annotation, but on a
  cron, so patch nights do not need a human annotating hosts.
- **More shipped profiles** (k3s/RKE2, Talos is likely out of scope — no SSH).
- **DRA device modeling** — host classes declaring their expected device
  inventory (driver/pool/attributes), surfaced as
  `InstanceType.DynamicResources` templates so DRA-requesting pods can drive
  scale-up; plus the `resource.k8s.io` RBAC karpenter's DRA path needs.
  Deliberately parked until karpenter's DRA support leaves its
  `--ignore-dra-requests` pre-GA phase — the provider-facing surface is still
  moving.
- **A signed manifest for verified execution.** Today a signature covers the
  script bytes alone, so an old signed script can be replayed and the `phase`
  header is not authenticated ([verified-exec](docs/verified-exec.md#what-this-does-not-close)).
  Signing a manifest that binds phase + `profile@version` + an expiry closes
  replay/rollback without giving up offline signing.

## Not planned

- **Machine provisioning.** If your infrastructure can create machines on
  demand, use a provider that does (cloud, proxmox, Cluster API). This project
  exists for fixed fleets where SSH is the only interface you get.
- **A karpenter fork.** Core is a library. Behavior differences belong in the
  CloudProvider surface or upstream, never in patched core.
- **Cloud-specific logic in the provider.** Everything EKS-specific lives in the
  `nodeadm-ssm` join profile, and it stays that way — the provider is AWS-free.
