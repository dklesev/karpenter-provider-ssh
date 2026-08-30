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

package sshexec

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// shimPath is kpssh-shim relative to this package (internal/sshexec).
var shimPath = filepath.Join("..", "..", "shim", "kpssh-shim")

// signer holds an ephemeral SSHSIG signing key and its allowed_signers file.
type signer struct {
	keyFile        string
	allowedSigners string
}

func newSigner(t *testing.T) signer {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "signer")
	mustRun(t, "ssh-keygen", "-q", "-t", "ed25519", "-f", key, "-N", "", "-C", "kpssh")
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatalf("read signer pub: %v", err)
	}
	fields := strings.Fields(string(pub))
	if len(fields) < 2 {
		t.Fatalf("malformed signer pub: %q", pub)
	}
	as := filepath.Join(dir, "allowed_signers")
	line := fmt.Sprintf("kpssh namespaces=\"kpssh\" %s %s\n", fields[0], fields[1])
	if err := os.WriteFile(as, []byte(line), 0o644); err != nil {
		t.Fatalf("write allowed_signers: %v", err)
	}
	return signer{keyFile: key, allowedSigners: as}
}

// sign returns the armored SSHSIG of script in the kpssh namespace.
func (s signer) sign(t *testing.T, script []byte) []byte {
	t.Helper()
	f := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(f, script, 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	mustRun(t, "ssh-keygen", "-Y", "sign", "-q", "-f", s.keyFile, "-n", "kpssh", f)
	sig, err := os.ReadFile(f + ".sig")
	if err != nil {
		t.Fatalf("read sig: %v", err)
	}
	return sig
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// runShim pipes wire to kpssh-shim (sudo disabled) and returns output + code.
func runShim(t *testing.T, allowedSigners string, wire []byte) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", shimPath)
	cmd.Stdin = bytes.NewReader(wire)
	cmd.Env = append(os.Environ(),
		"KPSSH_SHIM_SUDO=0",
		"KPSSH_ALLOWED_SIGNERS="+allowedSigners,
		"KPSSH_NAMESPACE=kpssh",
		"KPSSH_PRINCIPAL=kpssh",
		"TMPDIR="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	return string(out), exitCode(err)
}

func requireShimTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ssh-keygen", "bash", "sh", "base64", "mktemp"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
}

func TestShimRunsSignedScript(t *testing.T) {
	requireShimTools(t)
	s := newSigner(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script := []byte(fmt.Sprintf("#!/usr/bin/env bash\nset -euo pipefail\necho \"HELLO ${KPSSH_VAR_x}\"\ntouch %q\n", marker))
	sig := s.sign(t, script)

	wire, err := Envelope{
		Phase:  PhaseJoin,
		Script: script,
		Sig:    sig,
		Params: map[string]string{"KPSSH_VAR_x": "world"},
	}.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	out, code := runShim(t, s.allowedSigners, wire)
	if code != 0 {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "HELLO world") {
		t.Errorf("output missing param expansion:\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("script did not run (marker absent): %v", err)
	}
}

func TestShimRejectsTamperedScript(t *testing.T) {
	requireShimTools(t)
	s := newSigner(t)
	marker := filepath.Join(t.TempDir(), "ran")
	orig := []byte(fmt.Sprintf("#!/usr/bin/env bash\ntouch %q\n", marker))
	sig := s.sign(t, orig)
	tampered := append(append([]byte{}, orig...), []byte("echo PWNED\n")...)

	wire, err := Envelope{Phase: PhaseJoin, Script: tampered, Sig: sig}.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, code := runShim(t, s.allowedSigners, wire)
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (signature rejected)\n%s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("tampered script executed — marker exists")
	}
}

func TestShimRejectsWrongSigner(t *testing.T) {
	requireShimTools(t)
	trusted := newSigner(t)
	evil := newSigner(t) // not in trusted.allowedSigners
	script := []byte("#!/usr/bin/env bash\ntrue\n")
	sig := evil.sign(t, script)

	wire, err := Envelope{Phase: PhaseJoin, Script: script, Sig: sig}.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, code := runShim(t, trusted.allowedSigners, wire)
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (unauthorized signer)\n%s", code, out)
	}
}

func TestShimRejectsMissingSignature(t *testing.T) {
	requireShimTools(t)
	s := newSigner(t)
	// Hand-craft a wire with no sig-b64 (Marshal would refuse to build this).
	wire := "kpssh-envelope: 1\nphase: join\nscript-b64: " +
		base64.StdEncoding.EncodeToString([]byte("echo hi\n")) + "\n"
	out, code := runShim(t, s.allowedSigners, []byte(wire))
	if code != 4 {
		t.Fatalf("exit=%d, want 4 (malformed: missing signature)\n%s", code, out)
	}
}

func TestShimRejectsBadParamName(t *testing.T) {
	requireShimTools(t)
	s := newSigner(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script := []byte(fmt.Sprintf("#!/usr/bin/env bash\ntouch %q\n", marker))
	sig := s.sign(t, script)
	// Hand-craft params with an invalid name (Marshal would refuse).
	params := base64.StdEncoding.EncodeToString([]byte("BAD-NAME=x\n"))
	wire := "kpssh-envelope: 1\nphase: join\n" +
		"params-b64: " + params + "\n" +
		"sig-b64: " + base64.StdEncoding.EncodeToString(sig) + "\n" +
		"script-b64: " + base64.StdEncoding.EncodeToString(script) + "\n"
	out, code := runShim(t, s.allowedSigners, []byte(wire))
	if code != 3 {
		t.Fatalf("exit=%d, want 3 (bad param)\n%s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("script ran despite bad param")
	}
}

// The param channel must not be an escalation channel. Every name here is a
// valid shell identifier — a bare-identifier rule would accept all of them —
// but each steers the interpreter rather than the script. BASH_ENV is the
// sharp one: bash expands it (running command substitutions) and sources the
// result before a non-interactive script, so this exact envelope would yield
// arbitrary root with a perfectly valid signature and exit 0. The shim is the
// authoritative gate, so it must reject these on its own, with no help from
// the controller.
func TestShimRejectsInterpreterControlParams(t *testing.T) {
	requireShimTools(t)
	for _, tc := range []struct{ name, param string }{
		{"BASH_ENV command substitution", `BASH_ENV=$(touch %q)`},
		{"ENV command substitution", `ENV=$(touch %q)`},
		{"LD_PRELOAD", `LD_PRELOAD=%q`},
		{"PATH hijack", `PATH=%q`},
		{"IFS", `IFS=%q`},
		{"unprefixed name", `EVIL=%q`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSigner(t)
			pwned := filepath.Join(t.TempDir(), "pwned")
			ran := filepath.Join(t.TempDir(), "ran")
			script := []byte(fmt.Sprintf("#!/usr/bin/env bash\ntouch %q\n", ran))
			sig := s.sign(t, script)

			// Hand-crafted: Marshal refuses to build these (see
			// TestMarshalRejectsInterpreterControlParams), but the shim must not
			// depend on that — assume a compromised or hostile controller.
			params := base64.StdEncoding.EncodeToString(
				[]byte(fmt.Sprintf(tc.param, pwned) + "\n"))
			wire := "kpssh-envelope: 1\nphase: join\n" +
				"params-b64: " + params + "\n" +
				"sig-b64: " + base64.StdEncoding.EncodeToString(sig) + "\n" +
				"script-b64: " + base64.StdEncoding.EncodeToString(script) + "\n"

			out, code := runShim(t, s.allowedSigners, []byte(wire))
			if code != 3 {
				t.Errorf("exit=%d, want 3 (bad param)\n%s", code, out)
			}
			if _, err := os.Stat(pwned); err == nil {
				t.Error("SECURITY: injected code executed — the param channel escalated past the signature gate")
			}
			if _, err := os.Stat(ran); err == nil {
				t.Error("signed script ran despite a rejected param")
			}
		})
	}
}

// A signed script's own failure must not masquerade as a shim protocol error:
// exit 2 from the script would otherwise read as "signature rejected".
func TestShimMapsScriptFailureOutOfReservedRange(t *testing.T) {
	requireShimTools(t)
	s := newSigner(t)
	script := []byte("#!/usr/bin/env bash\nexit 2\n")
	sig := s.sign(t, script)
	wire, err := Envelope{Phase: PhaseJoin, Script: script, Sig: sig}.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, code := runShim(t, s.allowedSigners, wire)
	if code != 10 {
		t.Fatalf("exit=%d, want 10 (script failure)\n%s", code, out)
	}
	if !strings.Contains(out, "script exited 2") {
		t.Errorf("real exit code not logged:\n%s", out)
	}
}

// A provisioning mistake (missing allowed_signers) still fails closed with
// exit 2, but the underlying ssh-keygen error must be logged — otherwise it is
// indistinguishable from a tampered script.
func TestShimLogsVerifyFailureReason(t *testing.T) {
	requireShimTools(t)
	s := newSigner(t)
	script := []byte("#!/usr/bin/env bash\ntrue\n")
	sig := s.sign(t, script)
	wire, err := Envelope{Phase: PhaseJoin, Script: script, Sig: sig}.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, code := runShim(t, filepath.Join(t.TempDir(), "does-not-exist"), wire)
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (fail closed)\n%s", code, out)
	}
	if !strings.Contains(out, "verify:") {
		t.Errorf("ssh-keygen stderr not logged on verify failure:\n%s", out)
	}
}

func TestShimRejectsUnknownPhase(t *testing.T) {
	requireShimTools(t)
	s := newSigner(t)
	out, code := runShim(t, s.allowedSigners, []byte("kpssh-envelope: 1\nphase: pwn\n"))
	if code != 5 {
		t.Fatalf("exit=%d, want 5 (unknown phase)\n%s", code, out)
	}
}

func TestShimProbe(t *testing.T) {
	requireShimTools(t)
	if runtime.GOOS != "linux" {
		t.Skip("probe reads /proc and runs nproc/systemctl — Linux only")
	}
	s := newSigner(t)
	out, code := runShim(t, s.allowedSigners, []byte("kpssh-envelope: 1\nphase: probe\n"))
	if code != 0 {
		t.Fatalf("exit=%d, want 0\n%s", code, out)
	}
	// First line is nproc's CPU count.
	if fields := strings.Fields(out); len(fields) == 0 {
		t.Errorf("probe produced no output")
	}
}
