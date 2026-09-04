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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The SSHSIG verifier and the host-key pin parser consume bytes that arrive
// from the API server (profile signatures, trusted-signer lines, pinned host
// keys) and gate every phase run — exactly the code that must neither panic
// nor mis-parse hostile input. Plain `go test` runs the seed corpus only;
// `make fuzz` drives the engine for a bounded time.

// fuzzSigner is an in-process ed25519 signer: the fuzz seeds must not depend
// on ssh-keygen being installed, unlike the cross-tool tests in sshsig_test.go.
func fuzzSigner(tb testing.TB) (ssh.Signer, ssh.PublicKey) {
	tb.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatalf("generate ed25519 key: %v", err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		tb.Fatalf("ssh signer: %v", err)
	}
	return s, s.PublicKey()
}

// FuzzVerifySSHSIG holds the one property that matters for a signature
// check: with a single trusted key that signed exactly one message, no other
// message may ever verify, whatever the fuzzer does to the armor. A mutated
// armor that still verifies the *seed* message is fine (PEM whitespace,
// trailing bytes after the signature string); one that verifies a different
// message is a forgery.
func FuzzVerifySSHSIG(f *testing.F) {
	signer, pub := fuzzSigner(f)
	msg := []byte("#!/usr/bin/env bash\necho hello\n")

	mh := sha256.Sum256(msg)
	var signed []byte
	signed = append(signed, sshsigMagic...)
	signed = appendSSHString(signed, []byte(DefaultSignNamespace))
	signed = appendSSHString(signed, nil) // reserved
	signed = appendSSHString(signed, []byte("sha256"))
	signed = appendSSHString(signed, mh[:])
	sig, err := signer.Sign(rand.Reader, signed)
	if err != nil {
		f.Fatalf("sign: %v", err)
	}
	armored := armorSSHSIG(f, pub, DefaultSignNamespace, "sha256", sig)
	// The baseline must verify, otherwise the property below is vacuous.
	if err := VerifySSHSIG(msg, armored, []ssh.PublicKey{pub}, ""); err != nil {
		f.Fatalf("seed signature rejected: %v", err)
	}

	f.Add(msg, armored)
	f.Add([]byte("tampered bytes"), armored)
	f.Add(msg, armorSSHSIG(f, pub, "other", "sha256", sig))
	f.Add(msg, armorSSHSIG(f, pub, DefaultSignNamespace, "sha512", sig))
	f.Add(msg, []byte("-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"))
	f.Add(msg, []byte{})

	f.Fuzz(func(t *testing.T, message, armor []byte) {
		err := VerifySSHSIG(message, armor, []ssh.PublicKey{pub}, "")
		if err == nil && !bytes.Equal(message, msg) {
			t.Fatalf("FORGERY: unsigned message %q verified under armor %q", message, armor)
		}
	})
}

// FuzzPinnedFingerprintFromKnownHost: every accepted input yields a
// "SHA256:" fingerprint and every rejected one yields none — the caller pins
// the host key on that string, so a silent empty success would disable the
// pin.
func FuzzPinnedFingerprintFromKnownHost(f *testing.F) {
	_, pub := fuzzSigner(f)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))

	f.Add(line)
	f.Add("host.example,10.0.0.1 " + line)
	f.Add("|1|c2FsdA==|aGFzaA== " + line)
	f.Add(Fingerprint(pub))
	f.Add("SHA256:")
	f.Add("ssh-ed25519 not-base64")
	f.Add("")

	f.Fuzz(func(t *testing.T, in string) {
		fp, err := PinnedFingerprintFromKnownHost([]byte(in))
		if err != nil {
			if fp != "" {
				t.Fatalf("error %v returned alongside fingerprint %q", err, fp)
			}
			return
		}
		if !strings.HasPrefix(fp, "SHA256:") {
			t.Fatalf("accepted %q but returned non-SHA256 fingerprint %q", in, fp)
		}
	})
}
