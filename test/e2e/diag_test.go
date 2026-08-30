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
	"context"
	"time"
)

// diagnostics dumps cluster + host state on failure, BEFORE teardown removes
// the evidence. Everything is best effort — a diagnostics failure must never
// mask the real one.
func (c *cluster) diagnostics() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second) // detached
	defer cancel()
	dump := func(desc, out string, err error) {
		if err != nil {
			c.t.Logf("---- %s (err: %v) ----\n%s", desc, err, out)
			return
		}
		c.t.Logf("---- %s ----\n%s", desc, out)
	}

	kc := []string{"--kubeconfig", c.kubeconfig}
	out, err := execRun(ctx, "kubectl", append(kc, "-n", c.ns, "get", "sshhosts", "-o", "wide")...)
	dump("sshhosts", out, err)
	out, err = execRun(ctx, "kubectl", append(kc, "get", "nodeclaims,nodes", "-o", "wide")...)
	dump("nodeclaims + nodes", out, err)
	out, err = execRun(ctx, "kubectl", append(kc, "-n", c.ns,
		"logs", "deploy/kpssh-karpenter-provider-ssh", "--tail=200")...)
	dump("controller logs (tail 200)", out, err)

	for _, h := range c.hosts {
		out, err = execRun(ctx, "docker", "exec", h.name, "journalctl", "-u", "kubelet", "--no-pager", "-n", "30")
		dump("journal kubelet "+h.name, out, err)
	}
}
