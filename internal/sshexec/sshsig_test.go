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
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func requireSSHKeygen(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
}

func (s signer) pub(t *testing.T) ssh.PublicKey {
	t.Helper()
	b, err := os.ReadFile(s.keyFile + ".pub")
	if err != nil {
		t.Fatalf("read pub: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(b)
	if err != nil {
		t.Fatalf("parse pub: %v", err)
	}
	return pub
}

// TestVerifySSHSIGValid is the cross-tool check: a signature produced by
// ssh-keygen must verify with the pure-Go implementation. If this passes the
// wire-format parsing matches OpenSSH exactly.
func TestVerifySSHSIGValid(t *testing.T) {
	requireSSHKeygen(t)
	s := newSigner(t)
	msg := []byte("#!/usr/bin/env bash\necho hello\n")
	sig := s.sign(t, msg)
	if err := VerifySSHSIG(msg, sig, []ssh.PublicKey{s.pub(t)}, "kpssh"); err != nil {
		t.Fatalf("valid ssh-keygen signature rejected: %v", err)
	}
	// default namespace resolves to kpssh
	if err := VerifySSHSIG(msg, sig, []ssh.PublicKey{s.pub(t)}, ""); err != nil {
		t.Fatalf("default namespace rejected: %v", err)
	}
}

// A real RSA signer, signing with SHA-1 "ssh-rsa", must be refused. This is
// the gate-parity test: PROTOCOL.sshsig prohibits ssh-rsa, so `ssh-keygen -Y
// verify` on the host rejects such a blob, and the controller must not be the
// more permissive of the two. x/crypto happily verifies it (its RSA key
// accepts ssh-rsa/rsa-sha2-256/rsa-sha2-512 alike), so without an explicit
// allow-list this passes.
func TestVerifySSHSIGRejectsSHA1RSA(t *testing.T) {
	requireSSHKeygen(t)
	dir := t.TempDir()
	key := filepath.Join(dir, "rsa")
	mustRun(t, "ssh-keygen", "-q", "-t", "rsa", "-b", "2048", "-f", key, "-N", "", "-C", "kpssh")

	pubBytes, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatalf("read rsa pub: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(pubBytes)
	if err != nil {
		t.Fatalf("parse rsa pub: %v", err)
	}
	privBytes, err := os.ReadFile(key)
	if err != nil {
		t.Fatalf("read rsa key: %v", err)
	}
	sk, err := ssh.ParsePrivateKey(privBytes)
	if err != nil {
		t.Fatalf("parse rsa key: %v", err)
	}

	msg := []byte("#!/usr/bin/env bash\necho hello\n")
	// Hand-build the SSHSIG the way ssh-keygen would, but sign the inner blob
	// with the legacy SHA-1 algorithm.
	mh := sha256.Sum256(msg)
	var signed []byte
	signed = append(signed, sshsigMagic...)
	signed = appendSSHString(signed, []byte("kpssh"))
	signed = appendSSHString(signed, nil)
	signed = appendSSHString(signed, []byte("sha256"))
	signed = appendSSHString(signed, mh[:])

	sig, err := sk.Sign(rand.Reader, signed) // plain ssh-rsa == SHA-1
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if sig.Format != ssh.KeyAlgoRSA {
		t.Skipf("signer produced %q, not legacy ssh-rsa — nothing to prove here", sig.Format)
	}

	armored := armorSSHSIG(t, pub, "kpssh", "sha256", sig)
	err = VerifySSHSIG(msg, armored, []ssh.PublicKey{pub}, "kpssh")
	if err == nil {
		t.Fatal("SECURITY: SHA-1 ssh-rsa signature accepted; ssh-keygen -Y verify would reject it")
	}
	if !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}
}

// armorSSHSIG wraps a signature into the PEM-armored SSHSIG wire format.
func armorSSHSIG(t testing.TB, pub ssh.PublicKey, namespace, hash string, sig *ssh.Signature) []byte {
	t.Helper()
	var inner []byte
	inner = appendSSHString(inner, []byte(sig.Format))
	inner = appendSSHString(inner, sig.Blob)

	var blob []byte
	blob = append(blob, sshsigMagic...)
	blob = binary.BigEndian.AppendUint32(blob, 1) // version
	blob = appendSSHString(blob, pub.Marshal())
	blob = appendSSHString(blob, []byte(namespace))
	blob = appendSSHString(blob, nil) // reserved
	blob = appendSSHString(blob, []byte(hash))
	blob = appendSSHString(blob, inner)

	return pem.EncodeToMemory(&pem.Block{Type: "SSH SIGNATURE", Bytes: blob})
}

func TestVerifySSHSIGTampered(t *testing.T) {
	requireSSHKeygen(t)
	s := newSigner(t)
	sig := s.sign(t, []byte("original bytes"))
	if err := VerifySSHSIG([]byte("tampered bytes"), sig, []ssh.PublicKey{s.pub(t)}, "kpssh"); err == nil {
		t.Fatal("tampered message accepted")
	}
}

func TestVerifySSHSIGUntrustedSigner(t *testing.T) {
	requireSSHKeygen(t)
	trusted := newSigner(t)
	evil := newSigner(t)
	msg := []byte("payload")
	sig := evil.sign(t, msg)
	err := VerifySSHSIG(msg, sig, []ssh.PublicKey{trusted.pub(t)}, "kpssh")
	if err == nil || !strings.Contains(err.Error(), "not in the trusted set") {
		t.Fatalf("untrusted signer accepted or wrong error: %v", err)
	}
}

func TestVerifySSHSIGWrongNamespace(t *testing.T) {
	requireSSHKeygen(t)
	s := newSigner(t)
	msg := []byte("payload")
	sig := s.sign(t, msg) // signed under namespace "kpssh"
	if err := VerifySSHSIG(msg, sig, []ssh.PublicKey{s.pub(t)}, "other"); err == nil {
		t.Fatal("signature from a different namespace accepted")
	}
}

func TestVerifySSHSIGNoSigners(t *testing.T) {
	requireSSHKeygen(t)
	s := newSigner(t)
	sig := s.sign(t, []byte("x"))
	if err := VerifySSHSIG([]byte("x"), sig, nil, "kpssh"); err == nil {
		t.Fatal("verification with an empty trusted set must fail closed")
	}
}

func TestParseTrustedSigners(t *testing.T) {
	requireSSHKeygen(t)
	s := newSigner(t)
	line, err := os.ReadFile(s.keyFile + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParseTrustedSigners([]string{string(line), "", "   "})
	if err != nil {
		t.Fatalf("ParseTrustedSigners: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1", len(keys))
	}
	if _, err := ParseTrustedSigners([]string{"not a key"}); err == nil {
		t.Error("expected error on garbage signer line")
	}
}
