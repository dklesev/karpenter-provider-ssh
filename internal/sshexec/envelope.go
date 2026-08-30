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
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// EnvelopeVersion is the wire-format version understood by kpssh-shim.
const EnvelopeVersion = "1"

// Phase names carried in an envelope. probe is read-only and unsigned; the
// mutating phases require a signature the shim verifies against a pinned signer.
const (
	PhaseProbe     = "probe"
	PhaseInstall   = "install"
	PhaseJoin      = "join"
	PhaseLeave     = "leave"
	PhaseUninstall = "uninstall"
)

func validPhase(p string) bool {
	switch p {
	case PhaseProbe, PhaseInstall, PhaseJoin, PhaseLeave, PhaseUninstall:
		return true
	}
	return false
}

// paramNameRE is the envelope's param-name grammar: the documented KPSSH_*
// contract, and deliberately stricter than envNameRE.
//
// envNameRE (a bare shell identifier) is the right rule for the Raw preamble,
// where the script is arbitrary root bash anyway and there is no boundary to
// cross. It is the WRONG rule here. Verified mode's whole claim is that a
// caller who cannot sign can only run signed code, and identifiers like
// BASH_ENV, ENV, SHELLOPTS, LD_PRELOAD, PATH and IFS steer the interpreter
// instead of the script — bash even command-substitutes $BASH_ENV and sources
// it before a non-interactive script runs. Every name the provider actually
// sends is KPSSH_* (see internal/profile.Env), so pinning the channel to that
// prefix costs nothing and removes the escalation entirely. The shim enforces
// the same rule and is the authoritative gate; this is the fail-fast half.
// The `+` matters: `*` would accept a bare "KPSSH_", which the shim's
// `KPSSH_?*` pattern refuses — the two gates must agree on the same grammar,
// even where disagreeing would only fail closed.
var paramNameRE = regexp.MustCompile(`^KPSSH_[A-Za-z0-9_]+$`)

// Envelope is one verified-execution operation streamed to kpssh-shim on
// stdin. Everything on the wire is untrusted: the shim re-verifies Sig over
// Script before it runs anything (see docs/verified-exec.md).
type Envelope struct {
	// Phase selects the operation (probe|install|join|leave|uninstall).
	Phase string
	// Script is the exact signed script bytes — byte-identical to what was
	// signed in CI, with no export preamble injected (that would break the
	// signature). Empty for probe.
	Script []byte
	// Sig is the armored SSHSIG (`-----BEGIN SSH SIGNATURE-----`) over Script.
	// Empty for probe.
	Sig []byte
	// Params are KPSSH_* variables the shim validates and exports before
	// executing Script. Names must match paramNameRE; values must not
	// contain a newline (the shim splits the param block per line).
	Params map[string]string
}

// Marshal renders the envelope wire format: a header of `field: base64value`
// lines, one per line, order-independent, terminated by EOF. Payloads are
// base64 (std, single line) so framing is binary-safe and collision-free.
func (e Envelope) Marshal() ([]byte, error) {
	if !validPhase(e.Phase) {
		return nil, fmt.Errorf("invalid phase %q", e.Phase)
	}
	if e.Phase != PhaseProbe && (len(e.Script) == 0 || len(e.Sig) == 0) {
		return nil, fmt.Errorf("phase %q requires script and signature", e.Phase)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "kpssh-envelope: %s\n", EnvelopeVersion)
	fmt.Fprintf(&b, "phase: %s\n", e.Phase)

	if len(e.Params) > 0 {
		var pb strings.Builder
		for _, k := range slices.Sorted(maps.Keys(e.Params)) {
			if !paramNameRE.MatchString(k) {
				return nil, fmt.Errorf("invalid param name %q: verified-exec params must match %s", k, paramNameRE)
			}
			if strings.ContainsRune(e.Params[k], '\n') {
				return nil, fmt.Errorf("param %q value contains a newline", k)
			}
			fmt.Fprintf(&pb, "%s=%s\n", k, e.Params[k])
		}
		fmt.Fprintf(&b, "params-b64: %s\n", base64.StdEncoding.EncodeToString([]byte(pb.String())))
	}
	if len(e.Sig) > 0 {
		fmt.Fprintf(&b, "sig-b64: %s\n", base64.StdEncoding.EncodeToString(e.Sig))
	}
	if len(e.Script) > 0 {
		fmt.Fprintf(&b, "script-b64: %s\n", base64.StdEncoding.EncodeToString(e.Script))
	}
	return []byte(b.String()), nil
}
