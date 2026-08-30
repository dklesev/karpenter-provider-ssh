# Installation

## Prerequisites

- Kubernetes 1.31+ with bootstrap-token auth (standard everywhere). CI
  exercises 1.34.
- `kubectl`, `git`, and **helm ≥ 3.13** — earlier helm versions reject an OCI
  chart reference without an explicit `--version`, which every command below
  omits. No Go toolchain is needed to install (only to develop).
- Pool hosts: SSH-reachable **from wherever the controller runs** (use the
  chart's `nodeSelector`/`tolerations` to place the controller somewhere with
  that reach), a user with passwordless sudo, outbound reach to the API
  endpoint. Nothing preinstalled.
- For `tls-bootstrap`: RBAC binding group `system:bootstrappers:kpssh` to
  node bootstrapping + client-CSR auto-approval; a kubelet-serving CSR
  approver (e.g. `postfinance/kubelet-csr-approver`) if you set
  `serverTLSBootstrap=true`.
- For `nodeadm-ssm` (EKS Hybrid): the cluster has `remoteNetworkConfig`, a
  HYBRID_LINUX access entry, and an SSM hybrid activation for cold joins.

## 1. karpenter.sh CRDs (before the controller)

`karpenter.sh` CRDs (NodePool, NodeClaim, NodeOverlay) are cluster-global and
**owned by whichever karpenter got there first**:

| cluster | what to do |
|---|---|
| greenfield (no karpenter) | `kubectl apply -f config/karpenter/` from a clone — the vendored karpenter.sh CRDs, pinned to the compiled library version. `make install` does the same plus the provider CRDs (needs Go) |
| existing karpenter (EKS, …) | **never touch karpenter.sh CRDs.** They are already there. `make install-shared` or the helm chart's `crds/` cover the provider CRDs |

Order matters: karpenter core is compiled into the controller, so it starts
informers on NodePool/NodeClaim at boot. Install the chart into a greenfield
cluster *before* these CRDs exist and the Pod crashloops until they appear.

The helm chart ships only the three `karpenter.dklesev.github.io` CRDs. Helm
installs `crds/` once and never upgrades them — on chart upgrades run
`kubectl apply -f charts/karpenter-provider-ssh/crds/` (or `make install-shared`).

## 2. Install the controller (helm)

```bash
helm install karpenter-provider-ssh \
  oci://ghcr.io/dklesev/charts/karpenter-provider-ssh \
  --namespace kpssh-system --create-namespace
```

Pin the chart version in anything automated (`--version <x.y.z>`, from the
[releases page](https://github.com/dklesev/karpenter-provider-ssh/releases)); the tagless
form above resolves to the newest tag and needs helm ≥ 3.13.

Useful values (full reference:
[chart README](https://github.com/dklesev/karpenter-provider-ssh/blob/main/charts/karpenter-provider-ssh/README.md)):

| value | why |
|---|---|
| `poolNamespace` | SSHHost inventory + secrets namespace, defaults to release namespace |
| `nodeSelector` / `tolerations` | controller scheduling — it must have SSH reach to the pool |
| `serviceMonitor.enabled` | Prometheus Operator scraping |
| `settings.logLevel` | `debug` while onboarding a pool pays for itself |
| `networkPolicy.enabled` | opt-in NetworkPolicy for the controller Pod (egress: API server, SSH to the pool; ingress: metrics) — leave it off unless the namespace is default-deny |
| `rbac.extraRules` | extra ClusterRole rules; the shipped set is scoped tight (see [security.md](security.md#controller-rbac)) and a custom join profile that talks to more APIs needs them added here |

Namespace note: the controller Pod is restricted-PSS-compliant. Workloads you
schedule onto pool nodes are your own business — if your cluster enforces PSS
per namespace, label accordingly.

## 3. Pool credentials

```bash
kubectl -n kpssh-system create secret generic pool-ssh-key \
  --from-file=privateKey=/path/to/pool_ed25519
# optional key "knownHost" pre-pins the host key; otherwise TOFU on first contact
```

## 4. Bootstrap RBAC (`tls-bootstrap` only)

`tls-bootstrap` mints a bootstrap token per join and the kubelet trades it for a
client certificate. A token has **no rights of its own**: something must bind the
`system:bootstrappers:kpssh` group to `system:node-bootstrapper` and auto-approve
the kubelet's CSR. That is what this manifest does.

```bash
kubectl apply -f examples/bootstrap-rbac.yaml
```

Skip it for `nodeadm-ssm` (EKS Hybrid Nodes), where the node identity comes from
SSM and IAM instead. Skipping it on a `tls-bootstrap` cluster is the single most
common cause of "join runs fine, node never appears" — the CSR is created and
never approved.

## 5. Inventory + classes

All examples live in [the examples directory](https://github.com/dklesev/karpenter-provider-ssh/tree/main/examples) — copy, edit, apply.

```bash
kubectl apply -f examples/profile-tls-bootstrap.yaml   # or profile-nodeadm-ssm.yaml
kubectl apply -f examples/host.yaml                    # one SSHHost per pool host
kubectl apply -f examples/nodeclass.yaml               # hostSelector + profile + price
kubectl apply -f examples/nodepool.yaml                # or nodepool-coexist.yaml (shared cluster)
```

Verify the pool before creating demand:

```bash
kubectl -n kpssh-system get sshhosts
# NAME     ADDRESS      CLASS   STATE       CLAIM   INSTALLED
# host-a   10.0.0.11    big     Available
```

`Available` means: SSH dialed, host key pinned, capacity observed. `Unhealthy`
with a dial timeout means the controller cannot reach the host over SSH —
fix the network path or schedule the controller onto a node that has one.

## 6. Smoke test

```bash
kubectl create deployment smoke --image=registry.k8s.io/pause:3.10 --replicas=0
kubectl set resources deployment smoke --requests=cpu=500m
kubectl patch deployment smoke --type merge -p '{
  "spec":{"template":{"spec":{
    "nodeSelector":{"karpenter.sh/nodepool":"<your-pool>"},
    "tolerations":[{"key":"karpenter.dklesev.github.io/pool","operator":"Exists","effect":"NoSchedule"}]
  }}}}'
kubectl scale deployment smoke --replicas=1
kubectl get nodeclaims -w   # claim → launched → registered → initialized
kubectl scale deployment smoke --replicas=0   # after consolidateAfter: node leaves, host Available
```

## EKS Hybrid Nodes walkthrough

The `nodeadm-ssm` profile implements the warm/cold split:

- **warm** (SSM registration + `/etc/nodeadm/nodeConfig.yaml` present): start
  containerd + kubelet. Seconds, same `mi-*`, consumes nothing.
- **cold** (blank host or after `nodeadm uninstall`): requires
  `KPSSH_SECRET_ACTIVATIONID` / `KPSSH_SECRET_ACTIVATIONCODE`, renders
  nodeConfig, runs `nodeadm install` + `nodeadm init`. Consumes one activation
  registration and mints a new `mi-*`.

Setup on top of the generic steps:

1. `SSHNodeClass`: `providerIDSource: Adopt`, pin `cluster.endpoint`/`caBundle`
   (hybrid nodes have no kube-public access by default), vars `clusterName`,
   `region`, `k8sVersion`.
2. Cold-join credentials via the generic Secret bridge:

   ```bash
   aws ssm create-activation --iam-role <hybrid-nodes-role> --registration-limit 10
   kubectl -n kpssh-system create secret generic eks-activation \
     --from-literal=activationId=… --from-literal=activationCode=…
   # SSHNodeClass.spec.joinSecretRef: {name: eks-activation}
   ```

   Activations expire (default 24 h, max 30 d) — keep the Secret current;
   how you do that is up to you, and warm joins never need it.
3. Billing check: node joined = billed, node removed = not billed. The leave
   sequence (core drains → the provider's `leave` stops kubelet → core deletes
   the Node) is exactly the billing-off sequence. Verify in Cost Explorer with
   daily granularity.

## Upgrades

```bash
helm upgrade karpenter-provider-ssh \
  oci://ghcr.io/dklesev/charts/karpenter-provider-ssh \
  -n kpssh-system                                     # same namespace as the install

# helm never upgrades crds/ — apply them from a checkout of the target version
kubectl apply -f charts/karpenter-provider-ssh/crds/
```

Omit `-n` and helm looks for the release in `default` and fails with *"has no
deployed releases"*.

The API is `v1beta1`: it changes, and not always additively. Breaking changes
are called out in the release notes with migration steps.

## Uninstall

```bash
# 1. drain the pool: scale demand to zero, wait for hosts → Available
# 2. remove instances of our CRDs, then the release
kubectl delete nodepool <ours>            # never touch foreign nodepools
kubectl -n kpssh-system delete sshhosts,sshnodeclasses,sshjoinprofiles --all
helm uninstall karpenter-provider-ssh -n kpssh-system
# 3. CRDs (only if nothing else uses them)
kubectl delete crd sshhosts.karpenter.dklesev.github.io \
  sshnodeclasses.karpenter.dklesev.github.io sshjoinprofiles.karpenter.dklesev.github.io
```

`karpenter.sh` CRDs: delete only on greenfield clusters where this provider
installed them.
