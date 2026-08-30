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

package profile

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

func testProfile(join string) *v1beta1.SSHJoinProfile {
	return &v1beta1.SSHJoinProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: v1beta1.SSHJoinProfileSpec{
			Version: "3",
			Scripts: v1beta1.ProfileScripts{
				Install: "echo install",
				Join:    join,
				Leave:   "echo leave",
			},
		},
	}
}

func TestRenderTemplating(t *testing.T) {
	p := testProfile("echo {{ .NodeName }} @ {{ .ClusterEndpoint }}")
	got, err := Render(p, PhaseJoin, &Context{NodeName: "node-1", ClusterEndpoint: "https://cp:6443"})
	if err != nil {
		t.Fatal(err)
	}
	want := "echo node-1 @ https://cp:6443"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderMissingKeyFails(t *testing.T) {
	p := testProfile("echo {{ .DoesNotExist }}")
	if _, err := Render(p, PhaseJoin, &Context{}); err == nil {
		t.Fatal("expected error for unknown template field")
	}
}

func TestRenderUnknownPhase(t *testing.T) {
	if _, err := Render(testProfile("x"), "reboot", &Context{}); err == nil {
		t.Fatal("expected error for unknown phase")
	}
}

func TestRenderEmptyScript(t *testing.T) {
	p := testProfile("x")
	if _, err := Render(p, PhaseUninstall, &Context{}); err == nil {
		t.Fatal("expected error for missing uninstall script")
	}
}

func TestEnvContract(t *testing.T) {
	c := &Context{
		ClusterEndpoint:    "https://cp:6443",
		ClusterCACert:      "Y2E=",
		BootstrapToken:     "abcdef.0123456789abcdef",
		NodeName:           "n",
		ProviderID:         "kpssh://ns/h",
		NodeLabels:         "a=b",
		RegisterWithTaints: "k=v:NoSchedule",
		HostAddress:        "10.0.0.1",
		NodeAddress:        "10.0.0.2",
		Vars:               map[string]string{"k8sMinor": "1.34"},
		Secrets:            map[string]string{"activation-id": "id123"},
	}
	env, err := c.Env()
	if err != nil {
		t.Fatal(err)
	}

	for k, want := range map[string]string{
		"KPSSH_CLUSTER_ENDPOINT": "https://cp:6443",
		"KPSSH_NODE_ADDRESS":     "10.0.0.2",
		"KPSSH_VAR_k8sMinor":     "1.34",
		// secret keys are uppercased with '-' → '_'
		"KPSSH_SECRET_ACTIVATION_ID": "id123",
	} {
		if env[k] != want {
			t.Errorf("env[%s] = %q, want %q", k, env[k], want)
		}
	}
	for k := range env {
		if !strings.HasPrefix(k, "KPSSH_") {
			t.Errorf("env key %s escapes the KPSSH_ namespace", k)
		}
	}
}

// Secret keys are normalized into shell identifiers, and distinct keys can
// collide ("a-b" and "a.b" → KPSSH_SECRET_A_B). Silently picking one by map
// iteration order would make the injected value change between reconciles.
func TestEnvRejectsCollidingSecretKeys(t *testing.T) {
	c := &Context{Secrets: map[string]string{"a-b": "one", "a.b": "two"}}
	if _, err := c.Env(); err == nil {
		t.Fatal("colliding secret keys must be an error, not a coin flip")
	}
}

// Script failures quote a stderr tail into the error, which is published as a
// NodeClaim event — a join script under `set -x` traces its secrets there.
func TestRedactRemovesSecretMaterial(t *testing.T) {
	c := &Context{
		BootstrapToken: "abcdef.0123456789abcdef",
		Secrets:        map[string]string{"activation-code": "s3cret-activation-code"},
	}
	msg := "+ export KPSSH_BOOTSTRAP_TOKEN=abcdef.0123456789abcdef\n" +
		"+ nodeadm init --code s3cret-activation-code\nexit status 1"
	got := c.Redact(msg)

	for _, leak := range []string{"abcdef.0123456789abcdef", "0123456789abcdef", "s3cret-activation-code"} {
		if strings.Contains(got, leak) {
			t.Errorf("redacted message still contains %q: %s", leak, got)
		}
	}
	if !strings.Contains(got, "nodeadm init") {
		t.Errorf("redaction ate the diagnostic context: %s", got)
	}
}

func TestValidate(t *testing.T) {
	valid := testProfile("x")
	if err := Validate(valid); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}

	t.Run("unparsable script", func(t *testing.T) {
		p := testProfile("x")
		p.Spec.Scripts.Join = "{{ .Unclosed "
		if err := Validate(p); err == nil {
			t.Fatal("expected a parse error")
		}
	})

	// Leave runs on the release path and in the zombie guard, neither of which
	// has a node class in hand — a leave reaching for .Vars fails exactly when
	// a host must be disconnected, and the guard then retries it forever.
	t.Run("leave depending on join-only data", func(t *testing.T) {
		p := testProfile("x")
		p.Spec.Scripts.Leave = "echo {{ .Vars.k8sMinor }}"
		if err := Validate(p); err == nil {
			t.Fatal("leave referencing .Vars must be rejected")
		}
	})
}

func TestInstalledMarker(t *testing.T) {
	if got := InstalledMarker(testProfile("x")); got != "test@3" {
		t.Fatalf("got %q, want test@3", got)
	}
	p := testProfile("x")
	p.Spec.Version = ""
	if got := InstalledMarker(p); got != "test@1" {
		t.Fatalf("empty version: got %q, want test@1", got)
	}
}
