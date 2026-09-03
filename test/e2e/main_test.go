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

// TestMain and the shared exec/util helpers for the e2e suite. The package doc
// lives in doc.go (untagged, so the package is non-empty for `go test ./...`).
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// kindNodeImageDefault pins the node image kind v0.32.0 ships. It is used
	// for BOTH the host image's FROM and every `kind create --image`, so the
	// pool host's kubelet is byte-identical to the control plane by
	// construction — no runtime derivation. Override with E2E_KIND_IMAGE.
	// Digest-pinned; mirrored as the ARG default in host-image/Dockerfile.
	kindNodeImageDefault = "kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"

	controllerImg = "kpssh-e2e/controller:dev"
	hostImg       = "kpssh-e2e/host:dev"

	// controllerNS is the release namespace the helm template deploys into.
	controllerNS = "kpssh-system"

	// signNamespace / signPrincipal must match the shim defaults
	// (shim/kpssh-shim) and sshexec.DefaultSignNamespace — the SSHSIG
	// namespace and the allowed_signers principal the verify is bound to.
	signNamespace = "kpssh"
	signPrincipal = "kpssh"
)

var (
	// repoRoot is the module root, derived from the test's working directory
	// (go test runs with cwd = the package dir, test/e2e).
	repoRoot string

	kindNodeImage string

	// clusterSem caps how many kind clusters build/run at once. Each cluster is
	// a control-plane container PLUS N privileged kubelet host containers, so on
	// a 2-vCPU CI runner more than a couple thrash the disk into
	// ephemeral-storage eviction. -parallel bounds Go's test concurrency;
	// this is the hard backstop regardless of -parallel. Override with
	// E2E_MAX_CLUSTERS.
	clusterSem chan struct{}
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	wd, err := os.Getwd()
	if err != nil {
		log.Printf("[e2e] getwd: %v", err)
		return 1
	}
	repoRoot = filepath.Clean(filepath.Join(wd, "..", ".."))
	kindNodeImage = envOr("E2E_KIND_IMAGE", kindNodeImageDefault)
	clusterSem = make(chan struct{}, envIntOr("E2E_MAX_CLUSTERS", 3))

	for _, bin := range []string{"docker", "kind", "kubectl", "helm", "ssh-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			log.Printf("[e2e] missing required binary %q: %v", bin, err)
			return 1
		}
	}

	// Build the two shared images once. They are read-only inputs to every
	// scenario; each cluster only `kind load`s them (cheap) into its own nodes.
	ctx := context.Background()
	log.Printf("[e2e] building controller image %s", controllerImg)
	if out, err := execRun(ctx, "docker", "build", "-t", controllerImg, repoRoot); err != nil {
		log.Printf("[e2e] controller image build failed: %v\n%s", err, tail(out, 40))
		return 1
	}
	log.Printf("[e2e] building pool-host image %s (base %s)", hostImg, kindNodeImage)
	if out, err := execRun(ctx, "docker", "build",
		"--build-arg", "BASE="+kindNodeImage,
		"-t", hostImg,
		filepath.Join(repoRoot, "test", "e2e", "host-image"),
	); err != nil {
		log.Printf("[e2e] host image build failed: %v\n%s", err, tail(out, 40))
		return 1
	}

	return m.Run()
}

// --------------------------------------------------------------------- exec

// execRun runs a command to completion, returning combined stdout+stderr. Used
// for infra tooling (kind, docker, helm, kubectl) that has no typed API.
func execRun(ctx context.Context, name string, args ...string) (string, error) {
	return execRunInput(ctx, "", name, args...)
}

// execRunInput is execRun with stdin fed from in.
func execRunInput(ctx context.Context, in, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if in != "" {
		cmd.Stdin = strings.NewReader(in)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// execOut captures stdout ONLY (stderr kept separate), for reads whose exact
// bytes matter — e.g. a jsonpath script value that gets signed.
func execOut(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, errb.String())
	}
	return out.String(), nil
}

// mustExec fails the test if the command errors, surfacing combined output.
func mustExec(ctx context.Context, t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := execRun(ctx, name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

// mustExecInput is mustExec with stdin.
func mustExecInput(ctx context.Context, t *testing.T, in, name string, args ...string) string {
	t.Helper()
	out, err := execRunInput(ctx, in, name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

// --------------------------------------------------------------------- util

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envIntOr(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// tail returns the last n lines of s (build-log trimming for failures).
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// stamp is a short unique-enough token for per-run resource names.
func stamp() string { return fmt.Sprintf("%d", time.Now().UnixNano()%1e6) }
