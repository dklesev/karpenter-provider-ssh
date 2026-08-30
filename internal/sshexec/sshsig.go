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
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/sets"
)

// SSHSIG verification in pure Go, mirroring `ssh-keygen -Y verify`, so the
// controller can prove a phase script is signed by a trusted signer before it
// ever connects — without shelling out to ssh-keygen or adding a dependency.
// The host's shim re-verifies independently; this is defense in depth and a
// fail-fast guard (see docs/verified-exec.md).
//
// Wire format (OpenSSH PROTOCOL.sshsig):
//
//	byte[6]  MAGIC "SSHSIG"
//	uint32   version (1)
//	string   publickey
//	string   namespace
//	string   reserved
//	string   hash_algorithm
//	string   signature
//
// The signed blob is MAGIC || namespace || reserved || hash_alg || H(message).

// DefaultSignNamespace is the SSHSIG namespace used across the project.
const DefaultSignNamespace = "kpssh"

var sshsigMagic = []byte("SSHSIG")

func readSSHString(b []byte) (val, rest []byte, err error) {
	if len(b) < 4 {
		return nil, nil, fmt.Errorf("truncated length prefix")
	}
	n := binary.BigEndian.Uint32(b[:4])
	b = b[4:]
	if int64(len(b)) < int64(n) {
		return nil, nil, fmt.Errorf("truncated string body")
	}
	return b[:n], b[n:], nil
}

func appendSSHString(dst, s []byte) []byte {
	var l [4]byte
	//nolint:gosec // length is bounded by CRD MaxLength; never exceeds uint32
	binary.BigEndian.PutUint32(l[:], uint32(len(s)))
	return append(append(dst, l[:]...), s...)
}

// ParseTrustedSigners parses OpenSSH public-key lines (authorized_keys /
// allowed_signers key column) into keys for VerifySSHSIG. Unparseable lines —
// and a list that yields no keys at all — are an error, so a malformed trust
// root fails loudly instead of silently disabling the check.
func ParseTrustedSigners(lines []string) ([]ssh.PublicKey, error) {
	var out []ssh.PublicKey
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(l))
		if err != nil {
			return nil, fmt.Errorf("parsing trusted signer %q: %w", l, err)
		}
		out = append(out, pub)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no trusted signers parsed")
	}
	return out, nil
}

// VerifySSHSIG verifies an armored SSHSIG over message, requiring the signing
// key to be one of allowed and the namespace to match (default "kpssh").
func VerifySSHSIG(message, armored []byte, allowed []ssh.PublicKey, namespace string) error {
	if namespace == "" {
		namespace = DefaultSignNamespace
	}
	if len(allowed) == 0 {
		return fmt.Errorf("sshsig: no trusted signers configured")
	}
	blk, _ := pem.Decode(armored)
	if blk == nil || blk.Type != "SSH SIGNATURE" {
		return fmt.Errorf("sshsig: not an armored SSH signature")
	}
	b := blk.Bytes
	if len(b) < 10 || !bytes.Equal(b[:6], sshsigMagic) {
		return fmt.Errorf("sshsig: bad magic")
	}
	if v := binary.BigEndian.Uint32(b[6:10]); v != 1 {
		return fmt.Errorf("sshsig: unsupported version %d", v)
	}
	rest := b[10:]
	pkBytes, rest, err := readSSHString(rest)
	if err != nil {
		return fmt.Errorf("sshsig: publickey: %w", err)
	}
	nsBytes, rest, err := readSSHString(rest)
	if err != nil {
		return fmt.Errorf("sshsig: namespace: %w", err)
	}
	_, rest, err = readSSHString(rest) // reserved
	if err != nil {
		return fmt.Errorf("sshsig: reserved: %w", err)
	}
	hashBytes, rest, err := readSSHString(rest)
	if err != nil {
		return fmt.Errorf("sshsig: hash_algorithm: %w", err)
	}
	sigBytes, _, err := readSSHString(rest)
	if err != nil {
		return fmt.Errorf("sshsig: signature: %w", err)
	}

	if string(nsBytes) != namespace {
		return fmt.Errorf("sshsig: namespace %q, want %q", nsBytes, namespace)
	}

	pub, err := ssh.ParsePublicKey(pkBytes)
	if err != nil {
		return fmt.Errorf("sshsig: parse publickey: %w", err)
	}
	authorized := false
	for _, a := range allowed {
		if a != nil && bytes.Equal(a.Marshal(), pub.Marshal()) {
			authorized = true
			break
		}
	}
	if !authorized {
		return fmt.Errorf("sshsig: signer not in the trusted set")
	}

	var mh []byte
	switch string(hashBytes) {
	case "sha256":
		s := sha256.Sum256(message)
		mh = s[:]
	case "sha512":
		s := sha512.Sum512(message)
		mh = s[:]
	default:
		return fmt.Errorf("sshsig: unsupported hash %q", hashBytes)
	}

	var signed []byte
	signed = append(signed, sshsigMagic...)
	signed = appendSSHString(signed, nsBytes)
	signed = appendSSHString(signed, nil) // reserved (empty)
	signed = appendSSHString(signed, hashBytes)
	signed = appendSSHString(signed, mh)

	sigFormat, srest, err := readSSHString(sigBytes)
	if err != nil {
		return fmt.Errorf("sshsig: inner signature format: %w", err)
	}
	sigBlob, _, err := readSSHString(srest)
	if err != nil {
		return fmt.Errorf("sshsig: inner signature blob: %w", err)
	}
	if !signatureAlgorithms.Has(string(sigFormat)) {
		return fmt.Errorf("sshsig: signature algorithm %q is not permitted", sigFormat)
	}
	if err := pub.Verify(signed, &ssh.Signature{Format: string(sigFormat), Blob: sigBlob}); err != nil {
		return fmt.Errorf("sshsig: signature verification failed: %w", err)
	}
	return nil
}

// signatureAlgorithms is the allow-list of inner signature formats.
//
// The format comes off the wire, and ssh.PublicKey.Verify is not an algorithm
// policy point: x/crypto's RSA key accepts anything in algorithmsForKeyFormat
// ("ssh-rsa"), which still includes SHA-1 "ssh-rsa". PROTOCOL.sshsig prohibits
// it outright, so `ssh-keygen -Y verify` on the host refuses a blob this
// verifier would accept — leaving the two gates disagreeing, with the
// controller as the weaker one. That is the wrong direction even though the
// SHA-1 exposure here is not practically forgeable: the signed preimage is a
// fixed ~66 bytes with no room to append collision blocks, and its only
// attacker-influenced region is SHA256(script), which can be sampled but not
// steered. Pin the set instead of relying on that argument holding.
var signatureAlgorithms = sets.New(
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoRSASHA256,
	ssh.KeyAlgoRSASHA512,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoSKED25519,
	ssh.KeyAlgoSKECDSA256,
)
