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
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// noopInstall replaces the shipped install on kindest/node hosts, where
// kubelet/containerd are preinstalled — join/leave stay verbatim as shipped
// (they are the contract under test).
const noopInstall = "#!/usr/bin/env bash\n" +
	"set -euxo pipefail\n" +
	"mkdir -p /var/lib/kpssh\n" +
	"echo \"kpssh install: kindest/node — binaries preinstalled, skipping\"\n"

// applyProfileRaw applies the shipped tls-bootstrap profile with install patched
// to a no-op.
func (c *cluster) applyProfileRaw() {
	c.t.Helper()
	mustExec(c.t.Context(), c.t, "kubectl", "--kubeconfig", c.kubeconfig, "apply",
		"-f", filepath.Join(repoRoot, "examples", "profile-tls-bootstrap.yaml"))
	c.patchProfile(map[string]any{"scripts": map[string]any{"install": noopInstall}})
}

// verifiedOpts steer how a Verified profile is built for the failure scenarios.
type verifiedOpts struct {
	// rogueJoin signs scripts.join with this signer instead of the pool signer.
	// Pair it with a trustedSigners list that includes the rogue key: the
	// controller-side check then passes, and the join must die at the host shim
	// (whose allowed_signers only ever holds the pool signer) — proof that the
	// host trust root stands alone.
	rogueJoin *signer
	// omitSignatures ships a Verified profile with no signatures block — the
	// controller must refuse before connecting (controller-side gate).
	omitSignatures bool
}

// applyProfileVerified applies the profile in verified form: install no-op +
// verbatim join/leave, signed over their EXACT stored bytes (read back via
// jsonpath so the signature covers precisely what the controller will send).
func (c *cluster) applyProfileVerified(opts verifiedOpts) {
	c.t.Helper()
	c.applyProfileRaw()

	if !opts.omitSignatures {
		sigs := map[string]any{}
		for _, phase := range []string{"install", "join", "leave"} {
			signWith := c.signer
			if phase == "join" && opts.rogueJoin != nil {
				signWith = opts.rogueJoin
			}
			sigs[phase] = signWith.sign(c.profileScript(phase))
		}
		c.patchProfile(map[string]any{"signatures": sigs})
	}
}

// profileScript returns the exact stored bytes of a phase script.
func (c *cluster) profileScript(phase string) string {
	c.t.Helper()
	out, err := execOut(c.t.Context(), "kubectl", "--kubeconfig", c.kubeconfig,
		"get", "sshjoinprofile", "tls-bootstrap", "-o", "jsonpath={.spec.scripts."+phase+"}")
	if err != nil {
		c.t.Fatalf("read scripts.%s: %v", phase, err)
	}
	return out
}

func (c *cluster) patchProfile(spec map[string]any) {
	c.t.Helper()
	payload, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		c.t.Fatalf("marshal profile patch: %v", err)
	}
	c.kubectl("patch", "sshjoinprofile", "tls-bootstrap", "--type", "merge", "-p", string(payload))
}

// applyNodeClassAndPool creates the SSHNodeClass and NodePool. consolidateAfter
// is a NodePool-global knob (why scenarios can't share a cluster): fast for the
// consolidation test, slow for the zombie test so the zombie path wins the race.
func (c *cluster) applyNodeClassAndPool(consolidateAfter string) {
	c.t.Helper()
	c.apply(fmt.Sprintf(`apiVersion: karpenter.dklesev.github.io/v1beta1
kind: SSHNodeClass
metadata:
  name: e2e
spec:
  joinProfileRef: {name: tls-bootstrap}
  providerIDSource: Static
  pricePerCPUHour: "0.02"
  vars:
    k8sMinor: "%s"
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: %s
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: %s
  template:
    metadata:
      labels: {karpenter.dklesev.github.io/managed: "true"}
    spec:
      nodeClassRef: {group: karpenter.dklesev.github.io, kind: SSHNodeClass, name: e2e}
      expireAfter: Never
      requirements:
        - {key: karpenter.sh/capacity-type, operator: In, values: ["on-demand"]}
  limits: {cpu: "4", memory: 4Gi}
`, k8sMinorFromImage(kindNodeImage), nodePoolName, consolidateAfter))
}

// hostSpec picks a pool host's exec posture.
type hostSpec struct {
	verified       bool     // Verified (user kpssh, shim) vs Raw (user root)
	trustedSigners []string // public-key lines for the CR; Verified requires at least one (CEL)
}

// applyHosts registers one SSHHost per started container.
func (c *cluster) applyHosts(spec hostSpec) {
	c.t.Helper()
	for i, h := range c.hosts {
		user, extra := "root", ""
		if spec.verified {
			user = "kpssh"
			extra = "  execMode: Verified\n  trustedSigners:\n"
			for _, s := range spec.trustedSigners {
				extra += fmt.Sprintf("    - %q\n", s)
			}
		}
		c.apply(fmt.Sprintf(`apiVersion: karpenter.dklesev.github.io/v1beta1
kind: SSHHost
metadata:
  name: host-%d
  namespace: %s
  labels: {karpenter.dklesev.github.io/host-class: e2e}
spec:
  address: %s
  user: %s
  sshKeySecretRef: {name: pool-ssh-key}
  capacity: {cpu: "2", memory: 2Gi}
%s`, i+1, c.ns, h.ip, user, extra))
	}
}

// createSmoke creates a pinned, pool-selecting deployment scaled to 0. Scenarios
// scale it to pull a node in / let it consolidate away.
func (c *cluster) createSmoke() {
	c.t.Helper()
	c.kubectl("create", "deployment", "smoke", "--image=registry.k8s.io/pause:3.10", "--replicas=0")
	c.kubectl("set", "resources", "deployment", "smoke", "--requests=cpu=300m")
	c.kubectl("patch", "deployment", "smoke", "--type", "merge", "-p",
		`{"spec":{"template":{"spec":{"nodeSelector":{"karpenter.sh/nodepool":"`+nodePoolName+`"}}}}}`)
}

func (c *cluster) scaleSmoke(replicas int) {
	c.t.Helper()
	c.kubectl("scale", "deployment", "smoke", fmt.Sprintf("--replicas=%d", replicas))
}

// k8sMinorFromImage parses "kindest/node:v1.34.0[@sha256:…]" -> "1.34". The
// pinned node image IS the control plane, so its tag is authoritative for the
// SSHNodeClass k8sMinor var.
func k8sMinorFromImage(img string) string {
	_, tag, _ := strings.Cut(img, ":")
	tag, _, _ = strings.Cut(tag, "@")
	tag = strings.TrimPrefix(tag, "v")
	parts := strings.SplitN(tag, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return tag
}
