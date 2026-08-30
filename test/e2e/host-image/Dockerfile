# /*
# Copyright The karpenter-provider-ssh Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
# */

# Pool-host stand-in for e2e: kindest/node + sshd.
#
# kindest/node is the battle-tested kubelet-in-docker environment — its
# entrypoint handles cgroup/mount setup, containerd is pre-tuned, and the
# CNI + pause images the cluster needs are pre-baked (no registry pulls from
# inside the host). kubelet/containerd binaries are preinstalled, so the e2e
# patches the profile's `install` phase to a no-op; `join`/`leave` run
# verbatim as shipped. `docker restart` is the reboot simulation.
#
# BASE must match the cluster's node image (same kubelet version as the
# control plane). TestMain passes the same pinned image it hands `kind create`,
# so the two are identical by construction rather than derived at runtime.
ARG BASE
FROM ${BASE}

ENV DEBIAN_FRONTEND=noninteractive
# sudo is NOT in kindest/node — the verified shim self-escalates with `sudo -n`
# (and its sudoers drop-in needs /etc/sudoers.d, which the sudo package creates).
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server sudo \
    && rm -rf /var/lib/apt/lists/*

# A pool host boots OUT of the cluster: no kubelet on startup, and kind's
# kubeadm drop-in must not override the profile's ExecStart drop-in. kind's
# unit also gates on ConditionPathExists=/var/lib/kubelet/config.yaml — the
# profile configures kubelet elsewhere, so systemd would silently SKIP the
# start; clear the condition via a drop-in.
RUN systemctl disable kubelet || true \
    && rm -rf /etc/systemd/system/kubelet.service.d \
    && mkdir -p /etc/systemd/system/kubelet.service.d \
    && printf '[Unit]\nConditionPathExists=\n' > /etc/systemd/system/kubelet.service.d/00-e2e-clear-conditions.conf \
    && rm -f /etc/kubernetes/kubelet.conf /etc/kubernetes/bootstrap-kubelet.conf

RUN mkdir -p /root/.ssh && chmod 700 /root/.ssh && systemctl enable ssh

# Verified-execution posture. Baked STATIC so a Verified host is provisioned
# exactly as a real operator would provision one (see docs/verified-exec.md):
#   - an unprivileged `kpssh` login user,
#   - NOPASSWD sudo to EXACTLY the shim binary (a one-entry command allow-list),
#   - sshd pins that user's every session to the shim via ForceCommand.
# The block is INERT for Raw scenarios: those log in as root and never match
# `Match User kpssh`. The shim binary (shim/kpssh-shim, a release
# artifact) and the per-test allowed_signers are injected at host start via
# `docker cp` — like authorized_keys — since the trusted signer is generated
# fresh per test run.
RUN useradd --create-home --shell /bin/bash kpssh \
    && install -d -m 700 -o kpssh -g kpssh /home/kpssh/.ssh \
    && install -d -m 755 /opt/kpssh \
    && install -d -m 755 /etc/kpssh \
    && printf 'kpssh ALL=(root) NOPASSWD: /opt/kpssh/kpssh-shim\n' > /etc/sudoers.d/kpssh \
    && chmod 0440 /etc/sudoers.d/kpssh \
    && printf '\nMatch User kpssh\n    ForceCommand /opt/kpssh/kpssh-shim\n    PermitTTY no\n    AllowTcpForwarding no\n' >> /etc/ssh/sshd_config
