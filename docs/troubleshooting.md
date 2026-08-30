# Troubleshooting

Symptom-indexed. `kubectl` output abbreviated; controller logs are structured
JSON — grep the `message` field.

## Pool / SSHHost

### SSHHost stuck `Unhealthy` — `dial tcp <ip>:22: i/o timeout`
The controller cannot reach the host — from inside the controller Pod's
network path, not your laptop. Fix the network path, or schedule the
controller (`nodeSelector`/`tolerations`) onto a node that has SSH reach to
the pool.

### SSHHost `Unhealthy` — `host key mismatch`
TOFU pin vs. reinstalled/replaced host. If legitimate: clear
`status.hostKeyFingerprint` (or delete+recreate the SSHHost) after verifying
out-of-band. If not explainable — treat as MITM until proven otherwise.

### Host never leaves `Pending`
Probe hasn't succeeded yet: wrong `user`, missing `privateKey` key in the
Secret, sudo needs a TTY (`Defaults requiretty` — remove it), or port ≠ 22.
`status.lastProbeError` says which.

### Host stays `Claimed`/`InUse` but the NodeClaim is long gone
The probe controller releases dangling claims on its next pass. If it
persists: check controller logs for probe errors against that host —
release requires a successful reconcile.

## Provisioning

### Pod Pending, no NodeClaim
`kubectl get nodepool` — is ours `Ready`? Does the pod actually *select* the
pool (`nodeSelector: karpenter.sh/nodepool: <name>` + toleration for the pool
taint in the coexistence pattern)? Karpenter events on the pod
(`kubectl describe pod`) name the reason.

### `nodepool requirements filtered out all available instance types`
The pending pod's requirements (arch, labels, resources) fit no host class of
this pool — or all hosts of the fitting class are claimed/unhealthy. Check
`kubectl -n <pool-ns> get sshhosts` states and the class label vs. the
NodeClaim's `node.kubernetes.io/instance-type` requirement.

### NodeClaim created, join ran, but registration times out
`journalctl -u kubelet` on the host. Warm EKS path: was kubelet actually
started (SSM registration present?). TLS-bootstrap path: bootstrap RBAC
missing (`system:bootstrappers:kpssh` binding), wrong `k8sMinor`, or the
endpoint/CA handed to the host is unreachable *from the host*.

### `Adopt` mode: NodeClaim stuck `launched`, never `registered`
The provider waits (~4 min) for a Node whose InternalIP equals the host's
`nodeAddress`/`address`. If the node registers with a different IP (multiple
NICs), set `spec.nodeAddress` to the IP the node actually advertises.

## Coexistence

### Foreign NodePool names in our error logs (`SSHNodeClass "default" not found`)
Ownership-guard regression: karpenter core calls `GetInstanceTypes` for every
NodePool; the provider must return empty for pools whose `nodeClassRef` isn't
ours. Upgrade to the latest release; if it reappears, file a bug.

### Both karpenters try to serve the same pod
The pool NodePool is missing its distinguishing taint, or the pod tolerates
too much. Apply the consumer pattern from
[coexistence.md](coexistence.md#the-rules).

## EKS Hybrid specifics

### Warm rejoin suddenly cold / `nodeadm init` demands an activation
Someone ran `nodeadm uninstall` (or wiped `/var/lib/amazon/ssm/registration`).
That is the cold reset by definition — provide fresh activation credentials
via `joinSecretRef`.

### Warm-pool host rejoined the cluster by itself (after a reboot)
A leave that only *stops* kubelet leaves the unit enabled — on the next boot
systemd starts it, warm credentials still work, the node re-registers, and on
EKS Hybrid the billing window silently re-opens. The shipped profiles disable
kubelet **and** containerd on leave (and re-enable them on join) exactly for
this; custom profiles must do the same
([contract rule 3](join-profiles.md#lifecycle-contract)). The controller also
runs a zombie guard: the probe detects an active kubelet on an unclaimed host,
re-runs leave, and deletes the zombie Node (in that order — kubelet first, or
it re-creates the Node). If you see `zombie membership detected` in the logs,
fix the profile's leave script. A host is parked `Unhealthy` instead — never
force-left — when **no Node matches the host's IP and** it carries no
`installedProfile` marker: the provider does not touch machines it cannot
attribute to itself. (Both conditions are required. A host the provider *did*
install still gets force-left, marker or not, once a matching Node is found —
the marker alone is not what protects a foreign machine.)

### Billing didn't stop after scale-down
Billing follows the **Node object**, not kubelet. Verify the node is actually
gone (`kubectl get node <mi-…>` → NotFound). A NotReady node still bills. The
provider's leave handles the ordering (stop kubelet **then** delete Node) — if
you drove it manually in the wrong order, kubelet re-created the Node.

### Workload images won't pull on hybrid nodes (`connection reset by peer`)
On-prem nodes often have no egress to public registries (registry.k8s.io,
docker.io). Use your mirror + imagePullSecrets. This is a cluster/network
property, not a provider one — it just becomes visible the moment pods land on
pool nodes.

## Controller

### Out-of-cluster run panics: `unable to find leader election namespace`
Pass `--disable-leader-election` (dev runs only).

### `:8081 bind: address already in use`
Another controller-runtime process (dev karpenter, CAPI manager) holds the
probe port. Kill it or move one.

### Controller Pod unschedulable after pinning to a node
The pin (`nodeSelector`) and the default anti-affinity are ANDed — pinning to
a node that carries `karpenter.dklesev.github.io/managed` can't schedule. Pin to
a member node the provider does not manage.

### PSS violation warnings on install
The chart's Pod is restricted-compliant; warnings usually concern *your*
smoke/test workloads in an enforcing namespace. Run tests in the controller
namespace only if it's labeled appropriately.
