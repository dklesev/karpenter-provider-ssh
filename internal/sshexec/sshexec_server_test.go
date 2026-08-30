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
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// serverBehavior scripts how the in-process sshd answers a single exec request.
type serverBehavior struct {
	stdout     string
	stderr     string
	exitStatus uint32
	// hang accepts the exec and consumes stdin but never reports an
	// exit-status, emulating a stuck remote script.
	hang bool
	// rejectSessions refuses "session" channel opens, emulating a host with
	// sessions administratively disabled.
	rejectSessions bool
}

// testSSHServer is a minimal in-process SSH server: any public key
// authenticates, "session" channels accept one "exec" request, stdin is
// captured, and the scripted stdout/stderr/exit-status is replayed.
type testSSHServer struct {
	addr    string
	port    int32
	hostPub ssh.PublicKey

	behavior serverBehavior
	done     chan struct{} // closed at cleanup; unblocks the hang behavior
	wg       sync.WaitGroup

	mu       sync.Mutex
	conns    int
	sessions int
	stdin    []byte
	execCmd  string
	netConns []net.Conn
}

func startTestSSHServer(t *testing.T, behavior serverBehavior) *testSSHServer {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{
		// Accept any key: these tests exercise host-key pinning and exec
		// semantics, not client authentication.
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := &testSSHServer{
		addr:     "127.0.0.1",
		port:     int32(ln.Addr().(*net.TCPAddr).Port),
		hostPub:  hostSigner.PublicKey(),
		behavior: behavior,
		done:     make(chan struct{}),
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.netConns = append(s.netConns, c)
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.handleConn(c, cfg)
			}()
		}
	}()

	t.Cleanup(func() {
		close(s.done)
		_ = ln.Close()
		s.mu.Lock()
		for _, c := range s.netConns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}

func (s *testSSHServer) handleConn(c net.Conn, cfg *ssh.ServerConfig) {
	defer func() { _ = c.Close() }()
	conn, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return // e.g. the client aborted the handshake on host key mismatch
	}
	defer func() { _ = conn.Close() }()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ssh.DiscardRequests(reqs)
	}()
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		if s.behavior.rejectSessions {
			_ = newCh.Reject(ssh.Prohibited, "sessions disabled")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		s.mu.Lock()
		s.sessions++
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleSession(ch, chReqs)
		}()
	}
}

func (s *testSSHServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	for req := range reqs {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			continue
		}
		_ = req.Reply(true, nil)

		// The client copies its Stdin reader and closes the write side, so
		// this returns once the whole preamble+script has arrived.
		stdin, _ := io.ReadAll(ch)

		s.mu.Lock()
		s.execCmd = payload.Command
		s.stdin = stdin
		s.mu.Unlock()

		if s.behavior.hang {
			<-s.done
			return
		}
		if s.behavior.stdout != "" {
			_, _ = io.WriteString(ch, s.behavior.stdout)
		}
		if s.behavior.stderr != "" {
			_, _ = io.WriteString(ch.Stderr(), s.behavior.stderr)
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{s.behavior.exitStatus}))
		return
	}
}

func (s *testSSHServer) snapshot() (conns, sessions int, stdin, execCmd string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns, s.sessions, string(s.stdin), s.execCmd
}

func (s *testSSHServer) target(t *testing.T, user, pin string) Target {
	t.Helper()
	key, _ := testKeyPEM(t)
	return Target{Address: s.addr, Port: s.port, User: user, PrivateKey: key, PinnedHostKey: pin}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestRunHostKeyMismatchOpensNoSession(t *testing.T) {
	t.Parallel()
	srv := startTestSSHServer(t, serverBehavior{stdout: "should never run"})

	// Pin the fingerprint of an unrelated key so it cannot match.
	_, otherPub := testKeyPEM(t)
	pin := ssh.FingerprintSHA256(otherPub)

	_, err := Run(testContext(t), srv.target(t, "root", pin), "echo hi", nil)
	if err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("expected host key mismatch error, got %v", err)
	}
	conns, sessions, _, _ := srv.snapshot()
	if conns == 0 {
		t.Fatal("expected the client to reach the server")
	}
	if sessions != 0 {
		t.Fatalf("no session must be opened on host key mismatch, got %d", sessions)
	}
}

func TestRunPinnedHostKeyMatch(t *testing.T) {
	t.Parallel()
	srv := startTestSSHServer(t, serverBehavior{stdout: "out data", stderr: "err data"})

	pin := ssh.FingerprintSHA256(srv.hostPub)
	res, err := Run(testContext(t), srv.target(t, "root", pin), "echo hi", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.HostKeyFingerprint != pin {
		t.Errorf("HostKeyFingerprint = %q, want %q", res.HostKeyFingerprint, pin)
	}
	if res.Stdout != "out data" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "out data")
	}
	if res.Stderr != "err data" {
		t.Errorf("Stderr = %q, want %q", res.Stderr, "err data")
	}
}

func TestRunTOFUReturnsFingerprint(t *testing.T) {
	t.Parallel()
	srv := startTestSSHServer(t, serverBehavior{stdout: "ok"})

	res, err := Run(testContext(t), srv.target(t, "root", ""), "echo hi", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := ssh.FingerprintSHA256(srv.hostPub); res.HostKeyFingerprint != want {
		t.Errorf("HostKeyFingerprint = %q, want %q", res.HostKeyFingerprint, want)
	}
}

func TestRunEnvPreambleAndExecCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		user    string
		wantCmd string
	}{
		{name: "non-root user wraps in sudo", user: "ubuntu", wantCmd: "sudo bash -s"},
		{name: "root runs bash directly", user: "root", wantCmd: "bash -s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := startTestSSHServer(t, serverBehavior{})

			env := map[string]string{"B_VAR": "x y", "A_VAR": "it's"}
			if _, err := Run(testContext(t), srv.target(t, tc.user, ""), "echo done", env); err != nil {
				t.Fatalf("Run: %v", err)
			}

			_, _, stdin, execCmd := srv.snapshot()
			if execCmd != tc.wantCmd {
				t.Errorf("exec command = %q, want %q", execCmd, tc.wantCmd)
			}
			if !strings.HasPrefix(stdin, "set -euo pipefail\n") {
				t.Errorf("stdin must start with the pipefail preamble, got %q", stdin)
			}
			// Exports sorted (A_VAR before B_VAR), values single-quoted with
			// ' escaped as '\'' , script appended last.
			want := "set -euo pipefail\n" +
				"export A_VAR='it'\\''s'\n" +
				"export B_VAR='x y'\n" +
				"echo done"
			if stdin != want {
				t.Errorf("stdin mismatch:\ngot:  %q\nwant: %q", stdin, want)
			}
		})
	}
}

func TestRunInvalidEnvNameFailsBeforeDial(t *testing.T) {
	t.Parallel()
	srv := startTestSSHServer(t, serverBehavior{})

	_, err := Run(testContext(t), srv.target(t, "root", ""), "echo hi", map[string]string{"BAD-NAME": "v"})
	if err == nil || !strings.Contains(err.Error(), "invalid environment variable name") {
		t.Fatalf("expected env name validation error, got %v", err)
	}
	conns, _, _, _ := srv.snapshot()
	if conns != 0 {
		t.Fatalf("server saw %d connection(s); validation must fail before dialing", conns)
	}
}

func TestRunNonZeroExitWrapsStderrTail(t *testing.T) {
	t.Parallel()
	srv := startTestSSHServer(t, serverBehavior{stderr: "boom", exitStatus: 1})

	res, err := Run(testContext(t), srv.target(t, "root", ""), "exit 1", nil)
	if err == nil {
		t.Fatal("expected error for non-zero exit status")
	}
	if !strings.Contains(err.Error(), "script failed") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error must wrap the stderr tail, got %v", err)
	}
	if res == nil || res.Stderr != "boom" {
		t.Errorf("Result.Stderr = %+v, want %q", res, "boom")
	}
}

func TestRunInvalidPrivateKey(t *testing.T) {
	t.Parallel()
	_, err := Run(context.Background(), Target{Address: "192.0.2.1", PrivateKey: []byte("not a key")}, "true", nil)
	if err == nil || !strings.Contains(err.Error(), "parsing private key") {
		t.Fatalf("expected private key parse error, got %v", err)
	}
}

func TestRunSessionOpenRejected(t *testing.T) {
	t.Parallel()
	srv := startTestSSHServer(t, serverBehavior{rejectSessions: true})

	_, err := Run(testContext(t), srv.target(t, "root", ""), "echo hi", nil)
	if err == nil || !strings.Contains(err.Error(), "opening session") {
		t.Fatalf("expected session open error, got %v", err)
	}
}

func TestRunContextTimeoutReturnsPromptly(t *testing.T) {
	t.Parallel()
	srv := startTestSSHServer(t, serverBehavior{hang: true})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Run(ctx, srv.target(t, "root", ""), "sleep 600", nil)
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "script timed out") {
		t.Fatalf("expected script timed out error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Run took %v; must return promptly after the 500ms context deadline", elapsed)
	}
}
