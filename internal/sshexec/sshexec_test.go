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
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":        "'plain'",
		"with space":   "'with space'",
		"a'b":          `'a'\''b'`,
		"$(rm -rf /x)": "'$(rm -rf /x)'",
		"`id`":         "'`id`'",
		"":             "''",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestTail(t *testing.T) {
	if got := tail("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 20) + "END"
	got := tail(long, 5)
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "END") || len(got) > len("…")+5 {
		t.Errorf("tail = %q", got)
	}
}

func testKeyPEM(t *testing.T) ([]byte, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block), sshPub
}

func TestRunRejectsInvalidEnvNames(t *testing.T) {
	key, _ := testKeyPEM(t)
	// Names are a code channel in the export preamble; anything outside the
	// shell identifier grammar must be refused before any connection attempt.
	for _, name := range []string{"a b", "a;b", "a-b", "a.b", "1a", "$(x)", ""} {
		_, err := Run(context.Background(), Target{Address: "192.0.2.1", PrivateKey: key},
			"true", map[string]string{name: "v"})
		if err == nil || !strings.Contains(err.Error(), "invalid environment variable name") {
			t.Errorf("env name %q: expected validation error, got %v", name, err)
		}
	}
}

func TestRunValidEnvNamePassesValidation(t *testing.T) {
	key, _ := testKeyPEM(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// 127.0.0.1:1 refuses instantly — reaching the dial proves the env
	// preamble was accepted.
	_, err := Run(ctx, Target{Address: "127.0.0.1", Port: 1, PrivateKey: key},
		"true", map[string]string{"GOOD_NAME_1": "v"})
	if err == nil || !strings.Contains(err.Error(), "dialing") {
		t.Fatalf("expected dial error, got %v", err)
	}
}

func TestPinnedFingerprintFromKnownHost(t *testing.T) {
	_, pub := testKeyPEM(t)
	want := Fingerprint(pub)
	authorized := string(ssh.MarshalAuthorizedKey(pub))

	cases := map[string]string{
		"fingerprint passthrough": want,
		"authorized_keys line":    authorized,
		"known_hosts line":        "host1,10.0.0.1 " + authorized,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := PinnedFingerprintFromKnownHost([]byte(in))
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}

	for name, in := range map[string]string{"empty": "  ", "garbage": "not a key"} {
		t.Run(name, func(t *testing.T) {
			if _, err := PinnedFingerprintFromKnownHost([]byte(in)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestFingerprintFormat(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	fp := Fingerprint(sshPub)
	// ssh-keygen -lf format: SHA256:<43 base64 chars, unpadded>
	if !strings.HasPrefix(fp, "SHA256:") {
		t.Fatalf("missing prefix: %s", fp)
	}
	if strings.HasSuffix(fp, "=") {
		t.Fatalf("padding must be stripped: %s", fp)
	}
	if len(strings.TrimPrefix(fp, "SHA256:")) != 43 {
		t.Fatalf("unexpected digest length: %s", fp)
	}
}
