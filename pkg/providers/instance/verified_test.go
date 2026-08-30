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

package instance

import (
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dklesev/karpenter-provider-ssh/internal/profile"
	"github.com/dklesev/karpenter-provider-ssh/internal/sshexec"
	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

func verifiedHost() *v1beta1.SSHHost {
	return &v1beta1.SSHHost{
		ObjectMeta: metav1.ObjectMeta{Name: "h1"},
		Spec:       v1beta1.SSHHostSpec{ExecMode: v1beta1.ExecModeVerified, ShimCommand: "/opt/kpssh/kpssh-shim"},
	}
}

func signedProfile() *v1beta1.SSHJoinProfile {
	return &v1beta1.SSHJoinProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "p1"},
		Spec: v1beta1.SSHJoinProfileSpec{
			Scripts:    v1beta1.ProfileScripts{Join: "#!/bin/bash\necho ${KPSSH_VAR_x}\n"},
			Signatures: &v1beta1.ProfileSignatures{Join: "-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"},
		},
	}
}

func TestRunVerifiedPhaseBuildsEnvelope(t *testing.T) {
	var gotEnv sshexec.Envelope
	var gotTarget sshexec.Target
	fake := func(_ context.Context, tg sshexec.Target, env sshexec.Envelope) (*sshexec.Result, error) {
		gotTarget, gotEnv = tg, env
		return &sshexec.Result{}, nil
	}
	err := runVerifiedPhase(context.Background(), fake, sshexec.Target{Address: "1.2.3.4"},
		verifiedHost(), signedProfile(), profile.PhaseJoin, map[string]string{"KPSSH_VAR_x": "y"})
	if err != nil {
		t.Fatalf("runVerifiedPhase: %v", err)
	}
	if gotEnv.Phase != sshexec.PhaseJoin {
		t.Errorf("phase = %q, want join", gotEnv.Phase)
	}
	if string(gotEnv.Script) != "#!/bin/bash\necho ${KPSSH_VAR_x}\n" {
		t.Errorf("script forwarded verbatim? got %q", gotEnv.Script)
	}
	if len(gotEnv.Sig) == 0 {
		t.Error("signature not forwarded")
	}
	if gotEnv.Params["KPSSH_VAR_x"] != "y" {
		t.Errorf("params = %v", gotEnv.Params)
	}
	if gotTarget.ShimCommand != "/opt/kpssh/kpssh-shim" {
		t.Errorf("shim command = %q", gotTarget.ShimCommand)
	}
}

func TestRunVerifiedPhaseRejectsTemplates(t *testing.T) {
	fake := func(context.Context, sshexec.Target, sshexec.Envelope) (*sshexec.Result, error) {
		return &sshexec.Result{}, nil
	}
	prof := signedProfile()
	prof.Spec.Scripts.Join = "echo {{.Vars.x}}" // not template-free
	err := runVerifiedPhase(context.Background(), fake, sshexec.Target{}, verifiedHost(), prof, profile.PhaseJoin, nil)
	if err == nil || !strings.Contains(err.Error(), "template-free") {
		t.Fatalf("expected template-free rejection, got %v", err)
	}
}

func TestRunVerifiedPhaseRequiresSignature(t *testing.T) {
	fake := func(context.Context, sshexec.Target, sshexec.Envelope) (*sshexec.Result, error) {
		return &sshexec.Result{}, nil
	}
	prof := signedProfile()
	prof.Spec.Signatures = nil
	err := runVerifiedPhase(context.Background(), fake, sshexec.Target{}, verifiedHost(), prof, profile.PhaseJoin, nil)
	if err == nil || !strings.Contains(err.Error(), "no signature") {
		t.Fatalf("expected missing-signature rejection, got %v", err)
	}
	if !isConfigError(err) {
		t.Error("a missing signature is a profile problem and must classify as a config error")
	}
}

// TestRunVerifiedPhaseChecksSignatureBeforeConnect proves the controller-side
// check fires before the shim runner is ever invoked (no SSH), and that its
// failure classifies as a config error — the host must not be parked Unhealthy
// for a mis-signed profile.
func TestRunVerifiedPhaseChecksSignatureBeforeConnect(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	fake := func(context.Context, sshexec.Target, sshexec.Envelope) (*sshexec.Result, error) {
		called = true
		return &sshexec.Result{}, nil
	}
	h := verifiedHost()
	h.Spec.TrustedSigners = []string{string(ssh.MarshalAuthorizedKey(sshPub))}
	err = runVerifiedPhase(context.Background(), fake, sshexec.Target{}, h, signedProfile(), profile.PhaseJoin, nil)
	if err == nil || !strings.Contains(err.Error(), "controller-side signature check") {
		t.Fatalf("expected controller-side signature failure, got %v", err)
	}
	if called {
		t.Error("shim runner invoked despite a failed controller-side signature check")
	}
	if !isConfigError(err) {
		t.Error("a signature failure is a profile problem and must classify as a config error")
	}
}
