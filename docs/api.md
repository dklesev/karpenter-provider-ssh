# API reference — `karpenter.dklesev.github.io/v1beta1`

Three kinds. `SSHHost` is namespaced (the pool namespace, `POOL_NAMESPACE`);
`SSHNodeClass` and `SSHJoinProfile` are cluster-scoped.

Karpenter's own `NodePool`/`NodeClaim` are documented upstream
(<https://karpenter.sh/docs/concepts/nodepools/>); a NodePool joins this
provider by referencing an `SSHNodeClass` in `nodeClassRef`:

```yaml
nodeClassRef:
  group: karpenter.dklesev.github.io
  kind: SSHNodeClass
  name: my-pool
```

---

## SSHHost (namespaced, `kubectl get sshhosts`)

One inventory entry per pre-existing host.

```yaml
apiVersion: karpenter.dklesev.github.io/v1beta1
kind: SSHHost
metadata:
  name: host-a
  namespace: kpssh-system
  labels:
    karpenter.dklesev.github.io/host-class: big     # class membership (unset ⇒ "default")
spec:
  address: 10.0.0.11         # immutable
  port: 22
  user: ops
  sshKeySecretRef: {name: pool-ssh-key}
  nodeAddress: ""            # optional: IP literal the node advertises if != address
  capacity:                  # cpu and memory are mandatory
    cpu: "8"
    memory: 64Gi
```

### spec

| field | type | default | description |
|---|---|---|---|
| `address` | string | — | IP or DNS name the provider SSHes to. **Immutable** — it is the host's only identity, and repointing a claimed host would run its leave against a different machine. Re-IP = delete + recreate the SSHHost |
| `port` | int32 | `22` | SSH port (1–65535) |
| `user` | string | `root` | must run the profile scripts via passwordless sudo |
| `sshKeySecretRef.name` | string | — | Secret in the same namespace; keys `privateKey` (PEM, required) and `knownHost` (optional pre-pinned host key: a `SHA256:…` fingerprint, a `known_hosts` line, or a public key line — authoritative over the TOFU pin) |
| `nodeAddress` | string | `address` | IP the node advertises in-cluster (`kubelet --node-ip`); set when SSH uses a different path than pod traffic. Must be an **IP literal**: `--node-ip` takes no names, and the zombie guard matches it against Node InternalIPs |
| `capacity` | ResourceList | — | resources the host provides when joined. **`cpu` and `memory` are required** (CEL-validated); extended resources like `nvidia.com/gpu` are passed through. A class advertises the per-resource **minimum** across its hosts, so an undersized member shrinks the whole class — keep capacity uniform within a class |
| `execMode` | enum | `Raw` | `Raw` (`sudo bash -s`) or `Verified` (signed scripts via `kpssh-shim`, pinned by sshd `ForceCommand`). Set `Verified` only after provisioning the host for it. See [Verified execution](verified-exec.md) |
| `trustedSigners` | []string | — | Verified mode only, and then required (≥ 1 entry, CRD-enforced): OpenSSH public-key lines the controller checks profile signatures against before connecting (the host shim re-verifies independently). ≤ 16 entries |
| `shimCommand` | string | `/opt/kpssh/kpssh-shim` | Verified mode only: override the shim path invoked over SSH |

### Labels and annotations

| key | on | meaning |
|---|---|---|
| `karpenter.dklesev.github.io/host-class` | SSHHost | class membership — one class becomes one karpenter instance type. Unlabeled hosts land in the class `default`. All hosts of a class must share one **architecture**: a mixed-arch class is skipped with an error log (never advertised, never claimable) |
| `karpenter.dklesev.github.io/maintenance` | SSHHost (annotation) | present ⇒ host parked in `Maintenance`, never claimed (see [operations.md](operations.md#host-maintenance)) |
| `karpenter.dklesev.github.io/managed` | NodePool `spec.template.metadata.labels` → Node | **you set this, not the controller.** No Go code writes it: it is a convention the shipped NodePools follow (`examples/nodepool.yaml`), and the chart's default anti-affinity keys on it. A NodePool template without it produces nodes the controller may be scheduled onto — and consolidation then evicts the controller off its own node. Treat it as required on every NodePool that references an SSHNodeClass |
| `karpenter.dklesev.github.io/nodeclass-hash` (+`-version`) | NodeClaim (annotation) | stamped at Create with a digest of the nodeclass's join-affecting fields (`vars`, `joinSecretRef`, `providerIDSource`, `cluster`); a mismatch with the current spec marks the node `NodeClassDrift` for karpenter's drift disruption |
| `karpenter.dklesev.github.io/termination` | SSHNodeClass (finalizer) | blocks nodeclass deletion while NodeClaims still reference it |

#### Well-known labels the provider advertises

These are what a NodePool's `requirements` can select on. Anything else matches
nothing — a requirement the provider never advertises silently yields **zero
instance types**, which surfaces only as pods that never schedule.

| key | value | notes |
|---|---|---|
| `node.kubernetes.io/instance-type` | the host class | one class = one instance type |
| `kubernetes.io/arch` | `amd64` / `arm64` | the class's `observedArch`; mixed-arch classes are skipped entirely |
| `kubernetes.io/os` | `linux` | the only OS this provider joins |
| `topology.kubernetes.io/zone` | always the literal **`pool`** | a hybrid pool has no cloud zones. Put a real zone name in a NodePool requirement and nothing will ever match |
| `karpenter.sh/capacity-type` | always `on-demand` | pre-existing hosts are never spot. `examples/nodepool.yaml` pins this |

### status

| field | description |
|---|---|
| `state` | enum-validated: `Pending` → `Available` ⇄ `Claimed` → `InUse` → `Available`, plus `Unhealthy`, `Maintenance`, and `Leaving` (transient fence while the zombie guard disconnects a host) |
| `claimRef` | NodeClaim currently holding the host (`name` + `uid`) — **the claim lock**, written with compare-and-swap. The `uid` fences NodeClaim name reuse: a late Delete for a dead claim cannot tear down its successor's node |
| `installedProfile` | `<profile>@<version>` cache marker; install re-runs only when it differs |
| `bootstrapTokenID` | id of the kubelet bootstrap token minted for the in-flight join. The probe controller deletes the token Secret once the node registers (and clears this); the release path and the stale-claim path delete it if the join never got that far. Without it, spent token Secrets would pile up in `kube-system` forever — upstream's `tokencleaner` is disabled by default |
| `providerID` | externally-owned providerID after an `Adopt` join (e.g. `eks-hybrid:///…/mi-…`); cleared on leave |
| `hostKeyFingerprint` | TOFU-pinned SSH host key (SHA256); mismatch ⇒ hard connection failure |
| `observedCapacity` / `observedArch` | probe-reported facts, surfaced for drift-spotting against `spec.capacity`. `observedArch` is what the class advertises — a class with no probed host yet is not advertised at all |
| `observedGeneration` | spec generation the probe last acted on; a spec edit bypasses the probe-interval backoff |
| `lastProbeTime` / `lastProbeError` | probe bookkeeping |

---

## SSHNodeClass (cluster-scoped, `kubectl get snc`)

Karpenter node class: which hosts, how to join them, what they cost.

```yaml
apiVersion: karpenter.dklesev.github.io/v1beta1
kind: SSHNodeClass
metadata:
  name: hybrid-pool
spec:
  hostSelector:
    matchLabels: {karpenter.dklesev.github.io/host-class: big}   # optional narrowing
  joinProfileRef: {name: nodeadm-ssm}
  providerIDSource: Adopt          # Static | Adopt
  pricePerCPUHour: "0.02"
  joinSecretRef: {name: eks-activation}   # optional
  cluster:                          # optional discovery override
    endpoint: https://ABC.gr7.eu-central-1.eks.amazonaws.com
    caBundle: LS0tLS1CRUdJTi…
  vars:                             # profile template variables
    clusterName: my-cluster
    region: eu-central-1
    k8sVersion: "1.34"
```

### spec

| field | type | default | description |
|---|---|---|---|
| `hostSelector` | LabelSelector | all hosts in pool ns | limits claimable SSHHosts; karpenter still picks the cheapest fitting *class* within it |
| `joinProfileRef.name` | string | — | the `SSHJoinProfile` for hosts of this class |
| `vars` | map | `{}` | passed to scripts as `KPSSH_VAR_<key>`; keys must be valid shell identifiers (`[A-Za-z_][A-Za-z0-9_]*`, CEL-validated), values ≤ 2048 chars, ≤ 64 entries. **Not available to the `leave` script** — see the [leave contract](join-profiles.md#the-leave-contract-empty-render-context) |
| `joinSecretRef.name` | string | — | Secret in the pool namespace; each data key arrives as `KPSSH_SECRET_<UPPERCASED_KEY>` (`-` and `.`→`_`). Also not available to `leave`. How the Secret is produced and rotated is outside the provider's scope |
| `providerIDSource` | enum | `Static` | `Static`: provider mints `kpssh://<ns>/<host>` and hands it to the kubelet via `--provider-id` · `Adopt`: the join mechanism owns node identity (EKS nodeadm), and the provider adopts the registered Node's providerID by InternalIP match |
| `pricePerCPUHour` | string | `"0.02"` | USD per vCPU-hour while joined; drives karpenter's cost model and consolidation |
| `kubeReserved` | ResourceList | `80m` CPU, `300Mi` mem | kubelet overhead modeled per instance type (keys: `cpu`, `memory`, `ephemeral-storage`). Advertised to karpenter only — align the kubelet's `--kube-reserved` in the join profile |
| `maxPods` | int32 | `110` | pods capacity advertised per instance type (overrides a `pods` entry in `spec.capacity`). Advertised to karpenter only — align the kubelet's `maxPods` in the join profile |
| `cluster.endpoint` | string | cluster-info discovery | API server URL handed to joining kubelets (`https://…`); required when hosts can't read `kube-public/cluster-info` (EKS Hybrid) |
| `cluster.caBundle` | []byte | cluster-info discovery | cluster CA (PEM). A Kubernetes byte field: **base64-encoded string in YAML**, like `webhook.clientConfig.caBundle`. (It became `[]byte` in Go; the wire format did not change.) |

### status

`conditions` — operatorpkg readiness. `Ready=True` requires **all** of: the
referenced `SSHJoinProfile` exists, it validates, `hostSelector` parses, and it
matches at least one SSHHost. Referenced Secrets (`sshKeySecretRef`,
`joinSecretRef`) are **not** checked here — a missing Secret surfaces as a join
failure on the NodeClaim, not as a NotReady node class.

| `Ready=False` reason | meaning |
|---|---|
| `ProfileNotFound` | `joinProfileRef.name` resolves to nothing |
| `ProfileInvalid` | the profile's scripts do not parse, a required script is missing, or `leave` does not render against an empty context (see [join-profiles.md](join-profiles.md#the-leave-contract-empty-render-context)) |
| `SelectorError` | `hostSelector` is not a valid label selector |
| `NoHosts` | the selector matches no SSHHost in the pool namespace |

Karpenter refuses to launch NodeClaims against a node class that is not `Ready`
(`NodeClassNotReady`), so these four are the first thing to check when nothing
scales up.

---

## SSHJoinProfile (cluster-scoped, `kubectl get sjp`)

A named, versioned join mechanism: four idempotent scripts. Full contract,
environment table and authoring guide: [join-profiles.md](join-profiles.md).

```yaml
apiVersion: karpenter.dklesev.github.io/v1beta1
kind: SSHJoinProfile
metadata:
  name: tls-bootstrap
spec:
  version: "3"          # bump ⇒ invalidates every host's install cache
  scripts:
    install: |
      #!/usr/bin/env bash
      …
    join: |
      …
    leave: |
      …
    uninstall: |        # optional, manual/repair only
      …
```

### spec

| field | type | default | description |
|---|---|---|---|
| `version` | string | `"1"` | cache key: hosts store `<name>@<version>` in `status.installedProfile`; bumping re-runs `install` on next claim AND marks joined nodes `ProfileDrift` for karpenter's drift disruption (see [Operations](operations.md)). Pattern: `[A-Za-z0-9._-]{1,63}` |
| `scripts.install` | string | — | heavy one-time host preparation (packages, binaries); **idempotent**. ≤ 256 KiB |
| `scripts.join` | string | — | connect host to cluster (fast, every claim); **idempotent**. ≤ 256 KiB |
| `scripts.leave` | string | — | disconnect, keep installed components (fast, every release); **idempotent**. ≤ 256 KiB. **Must render with an empty context** — no `.Vars`, no `.Secrets` ([contract](join-profiles.md#the-leave-contract-empty-render-context)) |
| `scripts.uninstall` | string | `""` | full cleanup; never called automatically. ≤ 256 KiB |
| `timeouts.install` | duration | `10m` | deadline for the install script (slow bare-metal installs: raise it, but install+join must stay inside karpenter core's 15m registration TTL) |
| `timeouts.join` | duration | `5m` | deadline for the join script |
| `timeouts.leave` | duration | `3m` | deadline for the leave script |
| `signatures.{install,join,leave,uninstall}` | string | — | Verified mode: armored SSHSIG over the matching script (`ssh-keygen -Y sign -n kpssh`, offline). Required when a `Verified` host runs the phase; the script must be **template-free**. ≤ 16 KiB each. See [Verified execution](verified-exec.md) |

Timeouts are Go durations, pattern-validated as `^([0-9]+(s|m|h))+$` —
`90s`, `10m`, `1h30m` are accepted; `1.5h`, `500ms` and bare numbers are not.
The pattern is load-bearing, not cosmetic: an undecodable duration on a single
profile would break the typed List of every informer watching the kind and wedge
the controller cluster-wide.

Scripts are Go `text/template`s rendered with the join context, transported
over SSH and executed as `sudo bash -s` with the `KPSSH_*` environment
exported in a preamble (sshd `AcceptEnv` is typically locked down, so env
travels inside the script stream).

---

## providerID forms

| `providerIDSource` | form | owner |
|---|---|---|
| `Static` | `kpssh://<pool-ns>/<sshhost-name>` | this provider |
| `Adopt` | whatever the join mechanism sets, e.g. `eks-hybrid:///eu-central-1/<cluster>/mi-0abc…` | external (nodeadm) |
