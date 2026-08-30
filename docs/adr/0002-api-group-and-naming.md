# 0002 — API group `karpenter.dklesev.github.io`, prefix `kpssh`

**Status:** Accepted · **Date:** 2026-07-17

## Context

Everything an operator types or greps needs one consistent name: the API
group, the namespace, the shim, the script env params, the metrics, the sudo
user. An API group is effectively permanent once published — changing it is a
migration, not an edit — so the name had to be settled before publication.

The convention among Karpenter providers is clear: core is `karpenter.sh`; the
AWS provider is `karpenter.k8s.aws`; the Azure provider is
`karpenter.azure.com`. Providers name their group `karpenter.<vendor-domain>`.

A CAPI-style acronym (**C**luster **API** **P**rovider **SSH** → `capssh`) was
rejected: to exactly the audience this project targets — people who know CAPA
and CAPZ on sight — such a name announces the wrong project, while the docs
tell those same readers to go use Cluster API when they can provision machines.

## Decision

**Group: `karpenter.dklesev.github.io`.** The domain identifies the vendor; the
kind identifies the provider.

**Short prefix: `kpssh`** (Karpenter Provider SSH) for everything an operator
types or greps: `kpssh-system`, `kpssh-shim`, `KPSSH_*`, `kpssh_*` metrics, the
`kpssh` sudo user and SSHSIG namespace.

## On a second provider sharing the group

The obvious objection: if the same author writes a second Karpenter provider,
does it also live in `karpenter.dklesev.github.io`? Yes — and that is correct,
not a collision.

A CRD is keyed on **group + kind**. `SSHNodeClass` and a future
`HetznerNodeClass` coexist in one group because the kinds differ:

```
karpenter.dklesev.github.io
  SSHNodeClass       SSHHost       SSHJoinProfile     ← this repo
  HetznerNodeClass   HetznerServer                    ← a future one
```

This is precisely what Cluster API does: **every** infrastructure provider
shares `infrastructure.cluster.x-k8s.io` — `AWSCluster`, `AzureCluster`,
`DockerCluster` — across separate repos with separate release cycles.
`karpenter.k8s.aws` reads "karpenter, at AWS", not "the AWS karpenter provider";
AWS could add a second NodeClass kind there without minting a new group.

The cost, stated: two independently-released repos writing CRDs into one group
is a mild coordination smell — nothing enforces that they agree, and
`kubectl get crd` shows a mixed group. In practice kinds are distinct, there is
no conversion webhook to co-own, and the alternative (a group per provider,
`kpssh.dklesev.github.io` then `kphetzner.dklesev.github.io`) diverges from
every shipped Karpenter provider for no benefit.

## The kinds stay SSH-named

`SSHNodeClass`/`SSHHost`/`SSHJoinProfile` name the transport, not the problem,
and a `StaticNodeClass` with a `transport:` field was considered.

Rejected as speculative. The repo *is* `karpenter-provider-ssh`, SSH is what it
does today ([ADR 0001](0001-ssh-as-the-transport.md)), and abstracting over a
second transport that does not exist buys nothing now. If an agent transport
ever lands, the group model above already accommodates it as sibling kinds —
which is the same reasoning that made the group vendor-scoped.

## Consequences

- One prefix everywhere an operator touches the project: `kpssh-system`,
  `kpssh-shim`, `KPSSH_*`, the `kpssh` sudo user and SSHSIG namespace.
- Metric names follow the same rule with the subsystem naming a **domain**, not
  a controller: `kpssh_host_*`, `kpssh_instance_*`, `kpssh_pool_*`
  (`kpssh_hostprobe_*` would name the component that happens to record it).

## Revisit if

- A second provider under this domain turns out to need group-level isolation
  for a reason not anticipated here (a conversion webhook, say, or a group-scoped
  admission policy).
