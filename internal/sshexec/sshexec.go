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

// Package sshexec runs scripts on remote hosts over SSH with host-key pinning.
package sshexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"maps"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Target describes one SSH endpoint.
type Target struct {
	Address string
	Port    int32
	User    string
	// PrivateKey is the PEM-encoded client key.
	PrivateKey []byte
	// PinnedHostKey is the expected host key fingerprint ("SHA256:..."). Empty = TOFU
	// (first contact accepted, fingerprint returned for pinning by the caller).
	PinnedHostKey string
	// ShimCommand is the command run for verified execution (RunShim). A
	// ForceCommand-locked host ignores it and runs its pinned shim regardless;
	// sending it anyway lets the same call also drive hosts pinned via
	// authorized_keys command="". Empty defaults to DefaultShimCommand.
	ShimCommand string
}

// DefaultShimCommand is the conventional install path of kpssh-shim.
const DefaultShimCommand = "/opt/kpssh/kpssh-shim"

// Result of a remote execution.
type Result struct {
	Stdout string
	Stderr string
	// HostKeyFingerprint is the SHA256 fingerprint observed during the handshake.
	HostKeyFingerprint string
}

// Runner is the function type of Run; providers take it as a dependency so
// tests can substitute a fake executor.
type Runner func(ctx context.Context, t Target, script string, env map[string]string) (*Result, error)

// ShimRunner is the function type of RunShim; providers take it as a dependency
// so tests can substitute a fake verified executor.
type ShimRunner func(ctx context.Context, t Target, env Envelope) (*Result, error)

// Fingerprint returns the SHA256 fingerprint of an SSH public key, matching
// the format of `ssh-keygen -lf` ("SHA256:<base64-no-padding>").
func Fingerprint(key ssh.PublicKey) string {
	h := sha256.Sum256(key.Marshal())
	return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(h[:]), "=")
}

// PinnedFingerprintFromKnownHost parses pre-pinned host key material into a
// "SHA256:..." fingerprint. Accepted forms: a fingerprint itself, a
// known_hosts line, or an authorized_keys-style public key line.
func PinnedFingerprintFromKnownHost(data []byte) (string, error) {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", fmt.Errorf("empty host key material")
	}
	if strings.HasPrefix(s, "SHA256:") {
		return s, nil
	}
	if _, _, key, _, _, err := ssh.ParseKnownHosts([]byte(s + "\n")); err == nil {
		return Fingerprint(key), nil
	}
	if key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s)); err == nil {
		return Fingerprint(key), nil
	}
	return "", fmt.Errorf("host key material is neither a SHA256 fingerprint, a known_hosts line, nor a public key")
}

// envNameRE is the POSIX shell identifier grammar. Anything else in an env
// var name would either break the export preamble or, worse, smuggle shell
// syntax into a root shell — names are a code channel and MUST be validated,
// unlike values, which are single-quoted data.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// lockedBuffer is an io.Writer safe for concurrent use; the session goroutine
// keeps writing while the timeout path reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// effectiveUser resolves the login user, defaulting to root.
func effectiveUser(t Target) string {
	if t.User == "" {
		return "root"
	}
	return t.User
}

// connect dials the target, bounds the handshake, and enforces the host-key
// pin. The returned Result already carries the observed fingerprint (set even
// on a pin mismatch, so the caller can record it).
func connect(ctx context.Context, t Target) (*ssh.Client, *Result, error) {
	signer, err := ssh.ParsePrivateKey(t.PrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing private key: %w", err)
	}

	res := &Result{}
	hostKeyCallback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fp := Fingerprint(key)
		res.HostKeyFingerprint = fp
		if t.PinnedHostKey != "" && t.PinnedHostKey != fp {
			return fmt.Errorf("host key mismatch for %s: pinned %s, got %s", hostname, t.PinnedHostKey, fp)
		}
		return nil
	}

	port := t.Port
	if port == 0 {
		port = 22
	}
	cfg := &ssh.ClientConfig{
		User:            effectiveUser(t),
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         15 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", t.Address, port)
	dialer := net.Dialer{Timeout: cfg.Timeout}
	netConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, res, fmt.Errorf("dialing %s: %w", addr, err)
	}
	// ClientConfig.Timeout only covers ssh.Dial's TCP phase; the handshake in
	// NewClientConn is otherwise unbounded and runs before the ctx select in
	// runSession, so a tarpit host could hang the caller. Bound it with a conn
	// deadline.
	handshakeDeadline := time.Now().Add(cfg.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(handshakeDeadline) {
		handshakeDeadline = d
	}
	_ = netConn.SetDeadline(handshakeDeadline)
	conn, chans, reqs, err := ssh.NewClientConn(netConn, addr, cfg)
	if err != nil {
		_ = netConn.Close()
		return nil, res, fmt.Errorf("ssh handshake with %s: %w", addr, err)
	}
	_ = netConn.SetDeadline(time.Time{})
	return ssh.NewClient(conn, chans, reqs), res, nil
}

// runSession runs cmd on client, feeding stdin, bounded by ctx. res is filled
// with stdout/stderr in every outcome. On ctx cancellation the connection is
// torn down rather than signalled.
func runSession(ctx context.Context, client *ssh.Client, cmd, stdin string, res *Result) (*Result, error) {
	session, err := client.NewSession()
	if err != nil {
		return res, fmt.Errorf("opening session: %w", err)
	}
	defer func() { _ = session.Close() }()

	var stdout, stderr lockedBuffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	session.Stdin = strings.NewReader(stdin)

	done := make(chan error, 1)
	go func() { done <- session.Run(cmd) }()

	select {
	case <-ctx.Done():
		// Tear the connection down instead of signalling the remote command.
		// session.Signal writes a packet through the mux: on a wedged
		// connection — the host died mid-install, a NAT mapping expired — that
		// write blocks until the kernel abandons TCP retransmission, some 15
		// minutes past the deadline we are trying to enforce, pinning a probe
		// worker for the duration. sshd also ignores signal requests in common
		// configurations, while a closed connection reliably HUPs the command.
		// Closing unblocks session.Run in the goroutine below.
		_ = client.Close()
		res.Stdout, res.Stderr = stdout.String(), stderr.String()
		return res, fmt.Errorf("script timed out: %w", ctx.Err())
	case err := <-done:
		res.Stdout, res.Stderr = stdout.String(), stderr.String()
		if err != nil {
			return res, fmt.Errorf("script failed (stderr tail: %s): %w", tail(res.Stderr, 800), err)
		}
		return res, nil
	}
}

// Run executes script via `sudo bash -s` on the target, feeding the script on
// stdin. env is exported to the script. The context bounds the whole operation.
func Run(ctx context.Context, t Target, script string, env map[string]string) (*Result, error) {
	// Environment is injected as an export preamble: sshd's AcceptEnv is usually
	// locked down, so Setenv would silently not arrive. Validate names (a code
	// channel) before we even dial.
	var sb strings.Builder
	sb.WriteString("set -euo pipefail\n")
	for _, k := range slices.Sorted(maps.Keys(env)) {
		if !envNameRE.MatchString(k) {
			return nil, fmt.Errorf("invalid environment variable name %q", k)
		}
		fmt.Fprintf(&sb, "export %s=%s\n", k, shellQuote(env[k]))
	}
	sb.WriteString(script)

	client, res, err := connect(ctx, t)
	if err != nil {
		return res, err
	}
	defer func() { _ = client.Close() }()

	cmd := "sudo bash -s"
	if effectiveUser(t) == "root" {
		cmd = "bash -s"
	}
	return runSession(ctx, client, cmd, sb.String(), res)
}

// RunShim drives verified execution: it marshals the envelope and streams it to
// kpssh-shim (pinned on the host via ForceCommand), which verifies the phase
// script's signature against a trusted signer before running it. Unlike Run, no
// script bytes reach a shell in this process's control — the shim, not the
// controller, decides what executes. Param names are validated in
// Envelope.Marshal, before we dial.
func RunShim(ctx context.Context, t Target, env Envelope) (*Result, error) {
	wire, err := env.Marshal()
	if err != nil {
		return nil, fmt.Errorf("building envelope: %w", err)
	}
	client, res, err := connect(ctx, t)
	if err != nil {
		return res, err
	}
	defer func() { _ = client.Close() }()

	cmd := t.ShimCommand
	if cmd == "" {
		cmd = DefaultShimCommand
	}
	return runSession(ctx, client, cmd, string(wire), res)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
