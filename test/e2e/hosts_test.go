//go:build e2e

// Copyright The karpenter-provider-ssh Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// host is one pool-host container.
type host struct {
	name string // docker container name
	ip   string // address on the kind docker network
}

// startHosts launches n privileged systemd containers as pool hosts and wires
// each so it can serve BOTH exec modes: the pool key authorizes root (Raw) and
// kpssh (Verified) logins, the shim binary is installed, and the run's trusted
// signer is pinned in allowed_signers. Whether a given host runs Raw or Verified
// is then purely an SSHHost spec choice the scenario makes.
func (c *cluster) startHosts(n int) {
	t := c.t
	ctx := t.Context()

	// One SSH keypair per cluster — the controller authenticates with it; use a
	// dedicated key with access to nothing else.
	c.poolKeyPath = filepath.Join(t.TempDir(), "pool_key")
	mustExec(ctx, t, "ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-f", c.poolKeyPath)
	pubKey := c.poolKeyPath + ".pub"

	// allowed_signers for the run's signer, principal `kpssh` (matches the shim
	// default and the SSHSIG verify identity).
	allowedPath := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(allowedPath, []byte(c.signer.allowedLine()+"\n"), 0o600); err != nil {
		t.Fatalf("write allowed_signers: %v", err)
	}
	shimPath := filepath.Join(repoRoot, "shim", "kpssh-shim")

	flags := []string{
		"run", "-d", "--name", "", // name filled per-i
		"--privileged", "--cgroupns=private",
		"--tmpfs", "/run", "--tmpfs", "/tmp",
		"--volume", "/var",
		"--volume", "/lib/modules:/lib/modules:ro",
		"--network", "kind",
		hostImg,
	}
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("%s-host-%d", c.name, i)
		flags[3] = name
		mustExec(ctx, t, "docker", flags...)
		c.hosts = append(c.hosts, host{name: name})

		// SSH auth: pool public key trusts both logins. No bind mount — a mounted
		// file keeps the runner's uid and sshd StrictModes rejects it; copy+chown.
		mustExec(ctx, t, "docker", "cp", pubKey, name+":/root/.ssh/authorized_keys")
		mustExec(ctx, t, "docker", "cp", pubKey, name+":/home/kpssh/.ssh/authorized_keys")

		// Verified posture: the shim binary and the run's trusted signer.
		mustExec(ctx, t, "docker", "cp", shimPath, name+":/opt/kpssh/kpssh-shim")
		mustExec(ctx, t, "docker", "cp", allowedPath, name+":/etc/kpssh/allowed_signers")

		mustExec(ctx, t, "docker", "exec", name, "sh", "-c", strings.Join([]string{
			"chown root:root /root/.ssh/authorized_keys",
			"chmod 600 /root/.ssh/authorized_keys",
			"chown kpssh:kpssh /home/kpssh/.ssh/authorized_keys",
			"chmod 600 /home/kpssh/.ssh/authorized_keys",
			"chmod 0755 /opt/kpssh/kpssh-shim",
			"chown root:root /opt/kpssh/kpssh-shim /etc/kpssh/allowed_signers",
			"chmod 0644 /etc/kpssh/allowed_signers",
		}, " && "))
	}

	for i := range c.hosts {
		c.waitSSHD(&c.hosts[i])
		c.hosts[i].ip = c.hostIP(c.hosts[i].name)
	}
}

// waitSSHD blocks until the container's sshd is active.
func (c *cluster) waitSSHD(h *host) {
	c.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := execRun(c.t.Context(), "docker", "exec", h.name, "systemctl", "is-active", "ssh"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("sshd never came up on %s", h.name)
		}
		time.Sleep(2 * time.Second)
	}
}

// hostIP returns the container's address on the kind docker network.
func (c *cluster) hostIP(name string) string {
	out := mustExec(c.t.Context(), c.t, "docker", "inspect",
		"-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)
	ip := strings.TrimSpace(out)
	if ip == "" {
		c.t.Fatalf("no IP for container %s", name)
	}
	return ip
}
