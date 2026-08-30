# Single-board computers as a warm pool

Raspberry Pi and friends: boards you image once, that stay powered (or get
powered by something of yours), and that you want to join a cluster only while
pods need them. No PXE, no BMC, no provisioning API — SSH is what the board
offers, which is exactly this provider's contract. The full guide, with the
reasoning and the measured numbers from a real Pi 4, is
[docs/sbc.md](../../docs/sbc.md).

| file | what |
|---|---|
| [`prep-host.sh`](prep-host.sh) | one-time board prep over your existing login: pool user + key + sudo, memory cgroup (Pi OS boots with it off — needs one reboot), Wi-Fi power save off, swap off. Prints the SSHHost values for the board |
| [`cloud-init.yaml`](cloud-init.yaml) | the same at image time — merge into the `user-data` Raspberry Pi Imager writes to the boot partition |
| [`profile-sbc.yaml`](profile-sbc.yaml) | join profile: `tls-bootstrap` plus what Debian-family boards need (cgroup check, Debian's containerd CNI path, skip-apt-when-current, kubeadm drop-in ordering) |
| [`host-rpi.yaml`](host-rpi.yaml) | one `SSHHost` per board; class = board model |
| [`nodeclass-sbc.yaml`](nodeclass-sbc.yaml) | `SSHNodeClass`: profile, kubelet minor, price = electricity |
| [`nodepool-edge.yaml`](nodepool-edge.yaml) | tainted arm64 `NodePool`, slow consolidation |
| [`workload.yaml`](workload.yaml) | demand only a board can serve; its log proves DNS + API reachability from the board |
| [`kind-config.yaml`](kind-config.yaml), [`cilium-values.yaml`](cilium-values.yaml) | a laptop control plane the boards can join: kind is behind Docker's NAT, so the API server advertises the laptop address and Cilium/WireGuard carries pod traffic across the NAT. Not needed on a control plane the boards can route to |

## Sequence

```bash
# 0. a pool key, and the board prepared over the login you already have
ssh-keygen -t ed25519 -N '' -f pool_ed25519
ssh pi@192.168.1.216 "sudo env KPSSH_PUBKEY='$(cat pool_ed25519.pub)' bash -s" < examples/sbc/prep-host.sh
#    → "REBOOT REQUIRED" on a fresh Raspberry Pi OS: reboot it once

# 1. a control plane the board can reach — kind on this laptop:
export LAN_IP=$(ipconfig getifaddr en0)        # Linux: hostname -I | cut -d' ' -f1
sed "s/\${LAN_IP}/$LAN_IP/g" examples/sbc/kind-config.yaml   | kind create cluster --config -
sed "s/\${LAN_IP}/$LAN_IP/g" examples/sbc/cilium-values.yaml | helm upgrade --install cilium cilium/cilium \
  --version 1.19.6 -n kube-system -f - --wait
#    (any other control plane the board can route to: skip this step)

# 2. the provider, as in the README quick start
kubectl apply -f config/karpenter/
helm install karpenter-provider-ssh oci://ghcr.io/dklesev/charts/karpenter-provider-ssh \
  --namespace kpssh-system --create-namespace
kubectl -n kpssh-system create secret generic pool-ssh-key --from-file=privateKey=pool_ed25519
kubectl apply -f examples/bootstrap-rbac.yaml

# 3. the pool
export K8S_MINOR=$(kubectl version -o json | jq -r '.serverVersion | "\(.major).\(.minor|gsub("[^0-9]";""))"')
kubectl apply -f examples/sbc/profile-sbc.yaml
kubectl apply -f examples/sbc/host-rpi.yaml                      # edit address/capacity first
envsubst < examples/sbc/nodeclass-sbc.yaml | kubectl apply -f -
kubectl apply -f examples/sbc/nodepool-edge.yaml
kubectl -n kpssh-system get sshhosts                             # rpi-01 … Available

# 4. demand
kubectl apply -f examples/sbc/workload.yaml
kubectl scale deployment edge-probe --replicas=1
kubectl get nodeclaims -w                                        # → registered → initialized
kubectl logs deploy/edge-probe                                   # dns=ok api=v1.36.x, from the board
kubectl scale deployment edge-probe --replicas=0                 # consolidateAfter later: node leaves, host Available
```
