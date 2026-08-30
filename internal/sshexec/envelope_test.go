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
	"encoding/base64"
	"strings"
	"testing"
)

func TestEnvelopeMarshal(t *testing.T) {
	env := Envelope{
		Phase:  PhaseJoin,
		Script: []byte("#!/usr/bin/env bash\necho hi\n"),
		Sig:    []byte("-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"),
		Params: map[string]string{"KPSSH_VAR_x": "world", "KPSSH_NODE_NAME": "n1"},
	}
	out, err := env.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "kpssh-envelope: 1\n") {
		t.Errorf("missing version header:\n%s", s)
	}
	if !strings.Contains(s, "phase: join\n") {
		t.Errorf("missing phase:\n%s", s)
	}
	// params block decodes to sorted KEY=VALUE lines
	found := false
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if rest, ok := strings.CutPrefix(line, "params-b64: "); ok {
			found = true
			raw, err := base64.StdEncoding.DecodeString(rest)
			if err != nil {
				t.Fatalf("params not valid base64: %v", err)
			}
			want := "KPSSH_NODE_NAME=n1\nKPSSH_VAR_x=world\n"
			if string(raw) != want {
				t.Errorf("params block = %q, want %q", raw, want)
			}
		}
	}
	if !found {
		t.Errorf("no params-b64 line in envelope:\n%s", s)
	}
}

func TestEnvelopeProbeNeedsNoScript(t *testing.T) {
	out, err := Envelope{Phase: PhaseProbe}.Marshal()
	if err != nil {
		t.Fatalf("probe Marshal: %v", err)
	}
	if s := string(out); strings.Contains(s, "script-b64") || strings.Contains(s, "sig-b64") {
		t.Errorf("probe envelope must carry no script/sig:\n%s", s)
	}
}

func TestEnvelopeRejects(t *testing.T) {
	sig := []byte("sig")
	script := []byte("echo hi")
	cases := map[string]Envelope{
		"unknown phase":          {Phase: "pwn", Script: script, Sig: sig},
		"signed phase no sig":    {Phase: PhaseJoin, Script: script},
		"signed phase no script": {Phase: PhaseJoin, Sig: sig},
		"bad param name": {Phase: PhaseJoin, Script: script, Sig: sig,
			Params: map[string]string{"BAD-NAME": "x"}},
		"newline in value": {Phase: PhaseJoin, Script: script, Sig: sig,
			Params: map[string]string{"KPSSH_X": "a\nb"}},
	}
	for name, env := range cases {
		if _, err := env.Marshal(); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// Params must be pinned to the KPSSH_ prefix, not merely to shell-identifier
// syntax. Each name below is a legal identifier that controls the interpreter
// rather than the script; BASH_ENV in particular is command-substituted and
// sourced by bash before a non-interactive script, which would turn
// params-only access into arbitrary root behind a valid signature. This is the fail-fast
// half of the rule — the shim enforces it independently
// (TestShimRejectsInterpreterControlParams).
func TestMarshalRejectsInterpreterControlParams(t *testing.T) {
	for _, name := range []string{
		"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "PS4",
		"LD_PRELOAD", "LD_LIBRARY_PATH", "PATH", "IFS", "GLOBIGNORE",
		"HOME", "SHELL", "EVIL",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Envelope{
				Phase:  PhaseJoin,
				Script: []byte("echo hi"),
				Sig:    []byte("sig"),
				Params: map[string]string{name: "x"},
			}.Marshal()
			if err == nil {
				t.Errorf("SECURITY: Marshal accepted param %q — the param channel must be KPSSH_* only", name)
			}
		})
	}
}

// The prefix rule must not be so blunt that it breaks the names the provider
// actually sends (internal/profile.Env): fixed KPSSH_*, plus user vars and
// secrets, which arrive prefix-mangled as KPSSH_VAR_* / KPSSH_SECRET_*.
func TestMarshalAcceptsProviderParams(t *testing.T) {
	for _, name := range []string{
		"KPSSH_CLUSTER_ENDPOINT", "KPSSH_CLUSTER_CA_B64", "KPSSH_BOOTSTRAP_TOKEN",
		"KPSSH_NODE_NAME", "KPSSH_PROVIDER_ID", "KPSSH_NODE_LABELS", "KPSSH_TAINTS",
		"KPSSH_HOST_ADDRESS", "KPSSH_NODE_ADDRESS",
		"KPSSH_VAR_clusterName", "KPSSH_VAR_k8sMinor", "KPSSH_SECRET_ACTIVATION_CODE",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (Envelope{
				Phase:  PhaseJoin,
				Script: []byte("echo hi"),
				Sig:    []byte("sig"),
				Params: map[string]string{name: "x"},
			}).Marshal(); err != nil {
				t.Errorf("Marshal rejected a param the provider sends: %v", err)
			}
		})
	}
}
