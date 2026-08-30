# e2e

Full-loop tests against local [kind](https://kind.sigs.k8s.io) clusters with
**`kindest/node`-based containers as SSH pool hosts** — no cloud account, no
VMs, CI-runnable. A Go harness (`go test`, build tag `e2e`) owns the
parallelism, per-cluster lifecycle and assertions; the pool-host image and
`kind-config.yaml` stay declarative.

```bash
make e2e                              # all scenarios, parallel per-cluster
make e2e E2E_PARALLEL=6               # more concurrency (a beefy laptop)
make e2e-one E2E_RUN=TestE2EReboot    # one scenario, its own cluster
E2E_KEEP=1 make e2e-one E2E_RUN=…     # keep the cluster + hosts on failure
```

Requires `docker`, `kind`, `kubectl`, `helm`, `ssh-keygen`. The two shared
images (controller + pool host) build **once** in `TestMain`; each scenario
only `kind load`s them.

## One cluster per scenario

Each scenario runs in its **own ephemeral kind cluster** (`t.Parallel()` +
`t.Cleanup` teardown), never a shared one. karpenter core is cluster-global —
one consolidation/disruption loop, one singleton controller — so scenarios that
need conflicting NodePool disruption settings (consolidation wants `30s`, the
zombie guard wants `10m` so its path wins the race) or that force-delete
NodeClaims cannot share a cluster without serialising or cross-contaminating.
`E2E_MAX_CLUSTERS` (default = `E2E_PARALLEL`) caps how many are live at once —
each is a control plane plus privileged kubelet host containers, so the ceiling
is real on small machines.

One known flake under **local parallel runs**: every cluster's containers share
the one `kind` docker network, and `SSHHost.spec.address` is the host
container's IP recorded at creation. `TestE2EReboot`'s `docker restart` can
then come back on a different IP (a parallel scenario's churn re-assigned the
old one) and the host probes `Unhealthy` at the stale address until the wait
times out — the failure diag shows an empty kubelet journal, i.e. the
invariant under test actually held. Rerun it alone
(`make e2e-one E2E_RUN=TestE2EReboot`); CI is immune, one scenario per runner.

Local runs also silt up the **Docker Desktop VM disk** (volumes and build
cache, tens of GB after a few suites), and a full disk fails scenarios in a
misleading way: the join itself finishes in seconds, then *"timed out …
pool node Ready + smoke Running"* while the diag's kubelet journal shows
`Eviction manager: … ephemeral-storage` — the joined node is under disk
pressure and the workload never schedules. Same mechanism CI's "Free runner
disk" step exists for. `docker system df`, then
`docker builder prune -af && docker volume prune -f && docker container prune -f`.

## Scenarios

| scenario | asserts |
|---|---|
| `TestE2EScaleUp` | pending pod → NodeClaim → claim (CAS) → SSH join → node Ready → pod Running |
| `TestE2EConsolidation` | empty node drained + left; host `Available` again; **kubelet and containerd disabled** on the host (reboot-safety precondition) |
| `TestE2EReboot` | `docker restart` of a warm host (faithful reboot: systemd re-inits, enabled units start) → host must **not** rejoin; probes back to `Available` |
| `TestE2EZombieGuard` | NodeClaim force-deleted with finalizers stripped (provider `Delete` never runs, node keeps running) → probe detects the orphaned membership, force-runs `leave`, deletes the zombie Node |
| `TestE2EPowerOutage` | claimed host powered off (`docker stop`), NodeClaim deleted (stands in for node repair after its 10-min NotReady toleration) → leave fails against the dead host but the claim releases, host parks `Unhealthy`; `docker start` boots the still-enabled kubelet with warm credentials → zombie guard leaves + disables it, host `Available` again |
| `TestE2EVerifiedJoinAndLeave` | `execMode: Verified` host: probe, install, join **and** leave all flow through the `ForceCommand` shim; signed profile joins a Ready node then warm-releases |
| `TestE2EVerifiedRogueSignerRejectedByShim` | join signed by a key the CR's `trustedSigners` lists but the host's `allowed_signers` does not → the controller-side check passes, the correctly-signed install runs (pipeline works), the **host shim** rejects the rogue join, no node registers |
| `TestE2EVerifiedUnsignedRejectedByController` | `execMode: Verified` profile with no signatures → the **controller** refuses at the install phase before connecting; nothing runs, no node registers |

The reboot and zombie scenarios guard the costliest failure mode: a leave that
only *stops* kubelet leaves the unit enabled, and a rebooted warm host then
silently rejoins the cluster — on EKS Hybrid that re-opens the billing window.

The verified trio covers **both** gates of [verified execution](../../docs/verified-exec.md):
the rogue-signer case models a compromised controller (whoever writes CRs can
extend `trustedSigners` at will) and proves the host trust root stands alone;
the unsigned case proves the controller's pre-connect check.

## How the pool hosts work

`host-image/Dockerfile` extends the **cluster's own node image**
(`kindest/node`, pinned to the kind version's default) with sshd:

- battle-tested kubelet-in-docker environment — kind's entrypoint does the
  cgroup/mount setup, containerd is pre-tuned,
- the CNI and pause images the cluster schedules are **pre-baked** — no
  registry pulls from inside pool hosts,
- kubelet binary matches the control-plane version exactly,
- `docker restart` is a reboot: fresh systemd boot, enabled units start,
  filesystem (= installed state, credentials) persists.

Because kubelet/containerd are preinstalled, the harness patches the profile's
`install` to a no-op; **`join` and `leave` run verbatim as shipped** — they are
the contract under test. The image also clears kind's `ConditionPathExists`
gate on kubelet.service and disables the unit: a pool host boots *out* of the
cluster.

The image additionally bakes the **verified-execution posture** — an
unprivileged `kpssh` user, NOPASSWD sudo to exactly the shim, and an sshd
`ForceCommand` on that user — inert for Raw scenarios (they log in as root). The
shim binary and the per-run trusted signer are injected at host start (like
`authorized_keys`), since the signer is generated fresh per run. A host thus
serves either mode; the SSHHost spec picks.

## CI

CI runs this as the **release gate**, not on every PR. `.github/workflows/e2e.yaml`
fans the scenarios across a **matrix — one scenario per runner** (`fail-fast`
off), so each kind cluster gets a whole runner instead of several fighting for
one runner's disk. Privilege is split so PR-authored code never holds a
write-scoped token: a `prepare` job resolves the ref and sets the pending
status, the `scenarios` matrix runs the code with only `contents:read`, and a
`report` job sets the aggregate `e2e` commit status the release PR must satisfy.

It is triggered on the release-please PR (dispatched by `pr-gate` in
`release.yaml`); a maintainer can also comment `/e2e` on any PR. Feature PRs get
the `e2e` status set to success by `e2e-exempt.yaml` so it can stay a required
check on `main`.
