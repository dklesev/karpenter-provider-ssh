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
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	// Imported for its init(), which registers the provider types into the
	// global clientgoscheme.Scheme (see pkg/apis/v1beta1/doc.go) — that is why
	// the typed client below can decode SSHHost et al.
	_ "github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

// cluster is one ephemeral kind cluster plus its pool-host containers and a
// typed client. Its whole lifecycle (create, teardown, diagnostics) is bound to
// the owning *testing.T via t.Cleanup.
type cluster struct {
	t    *testing.T
	name string // kind cluster name, also the host-container prefix
	ns   string // controller release namespace

	kubeconfig string
	cl         client.Client

	hosts       []host  // pool-host containers, index 0 == host-1
	poolKeyPath string  // SSH private key the controller authenticates with
	signer      *signer // per-run offline signing identity (verified exec)
}

// newCluster brings up a fully wired but pool-EMPTY environment: kind + loaded
// images + CRDs + controller + N host containers (each usable Raw *or* Verified)
// + the pool SSH Secret + bootstrap RBAC. Scenarios then apply their own
// profile/nodeclass/nodepool/hosts on top. It blocks on the cluster semaphore
// so no more than E2E_MAX_CLUSTERS exist at once.
func newCluster(t *testing.T, nHosts int) *cluster {
	t.Helper()
	ctx := t.Context()

	clusterSem <- struct{}{}
	t.Cleanup(func() { <-clusterSem }) // released LAST (see cleanup ordering below)

	c := &cluster{
		t:          t,
		name:       clusterName(t.Name()),
		ns:         controllerNS,
		kubeconfig: filepath.Join(t.TempDir(), "kubeconfig"),
	}

	// Cleanup runs LIFO: register release (above) -> teardown -> diagnostics, so
	// on failure diagnostics dumps FIRST (cluster still alive), teardown next,
	// semaphore released last (a waiting scenario must not start while these
	// containers still hold disk).
	t.Cleanup(func() { c.teardown() })
	t.Cleanup(func() {
		if t.Failed() {
			c.diagnostics()
		}
	})

	c.logf("creating kind cluster (image %s)", kindNodeImage)
	mustExec(ctx, t, "kind", "create", "cluster",
		"--name", c.name,
		"--image", kindNodeImage,
		"--kubeconfig", c.kubeconfig,
		"--config", filepath.Join(repoRoot, "test", "e2e", "kind-config.yaml"),
		"--wait", "120s",
	)

	cfg, err := clientcmd.BuildConfigFromFlags("", c.kubeconfig)
	if err != nil {
		t.Fatalf("build restconfig: %v", err)
	}
	c.cl, err = client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	c.logf("loading images")
	mustExec(ctx, t, "kind", "load", "docker-image", controllerImg, "--name", c.name)
	mustExec(ctx, t, "kind", "load", "docker-image", hostImg, "--name", c.name)

	c.logf("installing CRDs + controller")
	mustExec(ctx, t, "kubectl", "--kubeconfig", c.kubeconfig, "apply",
		"-f", filepath.Join(repoRoot, "config", "karpenter"),
		"-f", filepath.Join(repoRoot, "config", "crd"))
	c.deployController()

	c.logf("provisioning %d pool host(s)", nHosts)
	c.signer = newSigner(t)
	c.startHosts(nHosts)

	// The controller authenticates with the pool key; both the Raw (root) and
	// Verified (kpssh) logins trust its public half — see startHosts.
	c.kubectl("create", "secret", "generic", "pool-ssh-key",
		"--from-file=privateKey="+c.poolKeyPath, "-n", c.ns)
	mustExec(ctx, t, "kubectl", "--kubeconfig", c.kubeconfig, "apply",
		"-f", filepath.Join(repoRoot, "examples", "bootstrap-rbac.yaml"))

	return c
}

// deployController renders the helm chart and waits for the rollout.
func (c *cluster) deployController() {
	ctx := c.t.Context()
	c.kubectl("create", "namespace", c.ns)
	rendered := mustExec(ctx, c.t, "helm", "template", "kpssh",
		filepath.Join(repoRoot, "charts", "karpenter-provider-ssh"),
		"--namespace", c.ns,
		"--set", "image.repository=kpssh-e2e/controller",
		"--set", "image.tag=dev",
		"--set", "image.pullPolicy=Never",
		"--set", "settings.logLevel=debug",
	)
	mustExecInput(ctx, c.t, rendered, "kubectl", "--kubeconfig", c.kubeconfig, "apply", "-f", "-")
	mustExec(ctx, c.t, "kubectl", "--kubeconfig", c.kubeconfig, "-n", c.ns,
		"rollout", "status", "deploy/kpssh-karpenter-provider-ssh", "--timeout=120s")
}

// kubectl runs kubectl against this cluster, failing the test on error.
func (c *cluster) kubectl(args ...string) string {
	c.t.Helper()
	return mustExec(c.t.Context(), c.t, "kubectl", append([]string{"--kubeconfig", c.kubeconfig}, args...)...)
}

// apply pipes a manifest into `kubectl apply -f -`.
func (c *cluster) apply(manifest string) {
	c.t.Helper()
	mustExecInput(c.t.Context(), c.t, manifest, "kubectl", "--kubeconfig", c.kubeconfig, "apply", "-f", "-")
}

// controllerLogs returns the last n lines of the controller log (best effort).
func (c *cluster) controllerLogs(n int) string {
	out, _ := execRun(c.t.Context(), "kubectl", "--kubeconfig", c.kubeconfig, "-n", c.ns,
		"logs", "deploy/kpssh-karpenter-provider-ssh", fmt.Sprintf("--tail=%d", n))
	return out
}

// teardown deletes the cluster and host containers unless E2E_KEEP=1.
func (c *cluster) teardown() {
	if envOr("E2E_KEEP", "") == "1" {
		c.logf("E2E_KEEP=1 — leaving cluster %q and its host containers", c.name)
		return
	}
	// Detached from t.Context (already cancelled at cleanup time on some paths).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, h := range c.hosts {
		_, _ = execRun(ctx, "docker", "rm", "-f", h.name)
	}
	_, _ = execRun(ctx, "kind", "delete", "cluster", "--name", c.name)
}

func (c *cluster) logf(format string, a ...any) {
	c.t.Logf("[%s] "+format, append([]any{c.name}, a...)...)
}

// clusterName builds a kind cluster name short enough that
// "<name>-control-plane" (the control-plane container's hostname) stays within
// the 63-char Linux hostname limit — sethostname fails with EINVAL otherwise,
// and container init dies. Budget the scenario slug against that ceiling.
func clusterName(testName string) string {
	const prefix = "kpssh-e2e-"
	const maxName = 46 // + len("-control-plane")=14 -> 60, safely < 63
	suffix := "-" + stamp()
	budget := maxName - len(prefix) - len(suffix)
	// Test funcs are TestE2E<Scenario>; drop the redundant "teste2e" so the
	// name reads kpssh-e2e-zombieguard-… , not kpssh-e2e-teste2ezombieguard-…
	slug := strings.TrimPrefix(sanitize(testName), "teste2e")
	if len(slug) > budget {
		slug = slug[:budget]
	}
	return prefix + slug + suffix
}

// sanitize maps a test name to a DNS-ish label fragment for cluster/container
// names (kind names must be lowercase alnum + '-').
func sanitize(s string) string {
	b := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b = append(b, r)
		case r >= 'A' && r <= 'Z':
			b = append(b, r+('a'-'A'))
		default:
			b = append(b, '-')
		}
	}
	return string(b)
}
