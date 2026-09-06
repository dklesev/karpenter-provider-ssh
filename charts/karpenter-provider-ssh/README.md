# karpenter-provider-ssh

Karpenter provider that autoscales cluster membership of pre-existing SSH-reachable hosts — for fleets where SSH is all you get. Generic over any TLS-bootstrap-capable control plane, with first-class EKS Hybrid Nodes support. Karpenter core is compiled in — this chart is the only controller you deploy.

## Install

<!-- x-release-please-start-version -->
```bash
helm install karpenter-provider-ssh \
  oci://ghcr.io/dklesev/charts/karpenter-provider-ssh \
  --version 1.0.3 \
  --namespace kpssh-system --create-namespace
```
<!-- x-release-please-end -->

**CRDs.** The chart ships the three `karpenter.dklesev.github.io` CRDs in `crds/`
(installed on first install, untouched on upgrade — update them explicitly with
`kubectl apply -f charts/karpenter-provider-ssh/crds/` when upgrading).
`karpenter.sh` CRDs (NodePool/NodeClaim/NodeOverlay) are **deliberately not
included**: on shared clusters they are owned by the existing karpenter
install; on greenfield clusters install them once from the pinned module
(`make install` / `config/karpenter/`).

**Placement.** The controller must be able to reach the pool hosts on their
SSH port — use `nodeSelector`/`tolerations` to schedule it somewhere with
that reach.

## Requirements

Kubernetes: `>=1.31.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{"nodeAffinity":{"requiredDuringSchedulingIgnoredDuringExecution":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"karpenter.dklesev.github.io/managed","operator":"DoesNotExist"}]}]}}}` | Affinity. The default forbids scheduling onto nodes this provider itself manages — consolidation would otherwise evict its own controller. |
| controller.env | list | `[]` | Extra environment variables for the controller container. |
| controller.healthProbePort | int | `8081` | Health/readiness probe port. |
| controller.metricsPort | int | `8080` | Metrics port (karpenter core registry + controller-runtime). |
| controller.resources | object | `{"requests":{"cpu":"200m","memory":"256Mi"}}` | Container resources. Requests only by default; the controller is a single Go binary and needs no limits to behave. |
| fullnameOverride | string | `""` | Override the fully qualified release name. |
| image.digest | string | `""` | Image digest. Takes precedence over tag when set. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/dklesev/karpenter-provider-ssh"` | Controller image repository. |
| image.tag | string | `""` | Image tag. Defaults to `v<Chart.appVersion>`. |
| imagePullSecrets | list | `[]` | Pull secrets for the controller image (only needed with a private mirror), e.g. `[{name: ghcr-pull}]`. |
| nameOverride | string | `""` | Override the chart name. |
| networkPolicy.apiServerCIDRs | list | `[]` | CIDRs the API server endpoints live in. Empty means any destination on `apiServerPorts` — narrow it when the endpoint IPs are stable. |
| networkPolicy.apiServerPorts | list | `[443,6443]` | TCP ports the API server answers on. 443 covers EKS and the in-cluster `kubernetes.default` ClusterIP; 6443 covers kubeadm/k0s-style endpoints. |
| networkPolicy.enabled | bool | `false` | Create a NetworkPolicy for the controller Pod. Egress is allow-listed to the API server, DNS and the SSH pool; ingress to the metrics/health ports. This pod holds the pool SSH key and mints bootstrap tokens — the policy bounds what a compromised controller can reach. Needs a policy-enforcing CNI (Calico, Cilium, …); with any other CNI it is silently inert. |
| networkPolicy.extraEgress | list | `[]` | Extra egress rules, appended verbatim (HTTP proxy, SSH bastion, a second SSH port, …). |
| networkPolicy.metricsFrom | list | `[]` | Ingress peers allowed to scrape the metrics port (verbatim NetworkPolicy peers). Empty means any source, e.g. `[{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: monitoring}}}]`. The health port stays open to all sources regardless — kubelet probes come from the node IP, which no pod/namespace selector can match. |
| networkPolicy.sshEgressCIDRs | list | `[]` | CIDRs the controller may open SSH connections to. Empty means *any* destination (0.0.0.0/0) on `sshPort` — set your pool subnets, e.g. `["10.0.0.0/16"]`. |
| networkPolicy.sshPort | int | `22` | SSH port allowed on egress. Must cover `SSHHost.spec.port` for every host in the pool; for a mixed-port pool add the rest via `extraEgress`. |
| nodeSelector | object | `{"kubernetes.io/os":"linux"}` | Node selector for the controller Pod. The controller needs SSH reach to every pool host — schedule it accordingly. |
| podAnnotations | object | `{}` | Extra annotations for the controller Pod. |
| podDisruptionBudget.enabled | bool | `false` | Create a PodDisruptionBudget. Only meaningful with `replicas` > 1 (leader failover); the chart refuses to render one at its floor (`minAvailable` >= `replicas`), which would permanently block node drains. |
| podDisruptionBudget.minAvailable | int | `1` | Minimum available controller pods. Integer only — must stay below `replicas`; percentages are rejected. |
| podLabels | object | `{}` | Extra labels for the controller Pod. |
| podSecurityContext | object | `{"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod-level security context (restricted-PSS compliant). |
| poolNamespace | string | `""` | Namespace holding SSHHost inventory, the SSH key Secret and join Secrets. Defaults to the release namespace. If set to a different namespace, that namespace must already exist. |
| priorityClassName | string | `"system-cluster-critical"` | PriorityClass for the controller. The provider is cluster infrastructure: losing it stops scale-up AND scale-down of the pool. |
| rbac.create | bool | `true` | Create ClusterRole/Roles and bindings. The rules are scoped: cluster-wide reads for scheduling state, writes only on karpenter.sh + kpssh APIs and Nodes, namespaced Roles for pool secrets, leader-election leases and bootstrap-token minting (kube-system). |
| rbac.extraRules | list | `[]` | Additional ClusterRole rules (escape hatch for custom join profiles that need more API access). |
| replicas | int | `1` | Number of controller replicas. Leader election is enabled, but >1 only buys faster failover — the pool claim lock is CAS-based either way. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}` | Container-level security context (restricted-PSS compliant). |
| service.annotations | object | `{}` | Extra annotations for the metrics Service. |
| service.enabled | bool | `true` | Create a ClusterIP Service for the metrics port. |
| serviceAccount.annotations | object | `{}` | Extra annotations for the ServiceAccount. |
| serviceAccount.create | bool | `true` | Create the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name. Defaults to the fullname. |
| serviceMonitor.additionalLabels | object | `{}` | Extra labels so your Prometheus instance selects the monitor. |
| serviceMonitor.enabled | bool | `false` | Create a Prometheus Operator ServiceMonitor. |
| serviceMonitor.interval | string | `"30s"` | Scrape interval. |
| serviceMonitor.metricRelabelings | list | `[]` | Metric relabelings. |
| settings.batchIdleDuration | string | `"1s"` | Idle gap that closes a batch early (BATCH_IDLE_DURATION). |
| settings.batchMaxDuration | string | `"10s"` | How long karpenter batches pending pods before provisioning (BATCH_MAX_DURATION). |
| settings.logLevel | string | `"info"` | Controller log level (debug, info, error). |
| terminationGracePeriodSeconds | int | `30` | Termination grace period. In-flight SSH phases finish or are retried by the next leader; scripts are idempotent by contract. |
| tolerations | list | `[]` | Tolerations for the controller Pod. |
| topologySpreadConstraints | list | `[]` | Topology spread constraints for the controller Pod. |

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| dklesev |  | <https://github.com/dklesev> |

## License

Apache-2.0 — see [LICENSE](../../LICENSE).
