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
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// signer is a throwaway offline signing identity for a run: a fresh ed25519 key
// whose public half is pinned in each host's allowed_signers and (optionally)
// in an SSHHost's trustedSigners. It signs profile scripts with stock
// `ssh-keygen -Y sign` in the `kpssh` namespace, exactly as an operator would
// in CI — never on the controller. The private key lives only in the test's
// temp dir.
type signer struct {
	t       *testing.T
	keyPath string
	pubLine string // "ssh-ed25519 AAAA... comment"
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	key := filepath.Join(t.TempDir(), "signer")
	mustExec(t.Context(), t, "ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-f", key)
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatalf("read signer pubkey: %v", err)
	}
	return &signer{t: t, keyPath: key, pubLine: strings.TrimSpace(string(pub))}
}

// allowedLine is the allowed_signers entry: the principal the shim verifies
// under (kpssh) followed by the public key.
func (s *signer) allowedLine() string { return signPrincipal + " " + s.pubLine }

// sign returns the armored SSHSIG over exactly these script bytes. Signing is
// over bytes (no templating), matching the verified-exec contract.
func (s *signer) sign(script string) string {
	s.t.Helper()
	// Capture stdout ONLY — the armored signature must not be polluted by any
	// stderr progress (‑q keeps it quiet, but keep the streams separate anyway).
	cmd := exec.CommandContext(s.t.Context(), "ssh-keygen", "-Y", "sign", "-q",
		"-f", s.keyPath, "-n", signNamespace)
	cmd.Stdin = strings.NewReader(script)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		s.t.Fatalf("ssh-keygen -Y sign: %v\n%s", err, errb.String())
	}
	return out.String()
}
