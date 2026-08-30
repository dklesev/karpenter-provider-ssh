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

// Package profile renders SSHJoinProfile phase scripts with the join context.
package profile

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/template"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

// Phase names.
const (
	PhaseInstall   = "install"
	PhaseJoin      = "join"
	PhaseLeave     = "leave"
	PhaseUninstall = "uninstall"
)

// Context is the data available to script templates. Everything is also exported
// as KPSSH_* environment variables to keep simple scripts template-free.
type Context struct {
	// ClusterEndpoint is the API server URL (https://host:port).
	ClusterEndpoint string
	// ClusterCACert is the base64-encoded cluster CA certificate (PEM).
	ClusterCACert string
	// BootstrapToken is a freshly minted "<id>.<secret>" bootstrap token (join phase only).
	BootstrapToken string
	// NodeName the kubelet should register as.
	NodeName string
	// ProviderID the kubelet should advertise (static mode; empty in adopt mode).
	ProviderID string
	// NodeLabels is a comma-separated k=v list for --node-labels.
	NodeLabels string
	// RegisterWithTaints is a comma-separated k=v:Effect list.
	RegisterWithTaints string
	// HostAddress is the SSHHost SSH address.
	HostAddress string
	// NodeAddress is the IP the node advertises in-cluster (kubelet --node-ip).
	NodeAddress string
	// Vars are the free-form variables from SSHNodeClass.spec.vars.
	Vars map[string]string
	// Secrets are injected as KPSSH_SECRET_<UPPERCASED_KEY> (from joinSecretRef).
	Secrets map[string]string
}

// secretEnvName maps a Secret data key to its env var name. Kubernetes allows
// '-' and '.' in keys; both become '_' so the result is a valid shell
// identifier.
func secretEnvName(key string) string {
	return "KPSSH_SECRET_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(key))
}

// Env converts the context to environment variables for the script.
// Vars keys are used as-is and validated at execution time (see sshexec).
//
// Distinct Secret keys can normalize to the same env var name ("a-b" and
// "a.b"); that would make the injected value depend on map iteration order, so
// it is an error rather than a coin flip.
func (c *Context) Env() (map[string]string, error) {
	env := map[string]string{
		"KPSSH_CLUSTER_ENDPOINT": c.ClusterEndpoint,
		"KPSSH_CLUSTER_CA_B64":   c.ClusterCACert,
		"KPSSH_BOOTSTRAP_TOKEN":  c.BootstrapToken,
		"KPSSH_NODE_NAME":        c.NodeName,
		"KPSSH_PROVIDER_ID":      c.ProviderID,
		"KPSSH_NODE_LABELS":      c.NodeLabels,
		"KPSSH_TAINTS":           c.RegisterWithTaints,
		"KPSSH_HOST_ADDRESS":     c.HostAddress,
		"KPSSH_NODE_ADDRESS":     c.NodeAddress,
	}
	for k, v := range c.Vars {
		env["KPSSH_VAR_"+k] = v
	}
	seen := map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(c.Secrets)) {
		name := secretEnvName(k)
		if prev, dup := seen[name]; dup {
			return nil, fmt.Errorf("secret keys %q and %q both map to %s", prev, k, name)
		}
		seen[name] = k
		env[name] = c.Secrets[k]
	}
	return env, nil
}

// redactionPlaceholder replaces secret material in messages that travel to
// events and logs.
const redactionPlaceholder = "[REDACTED]"

// minRedactableLen guards against a pathologically short secret value (say "1")
// redacting half the message.
const minRedactableLen = 8

// Redact removes secret material this context injected into the script's
// environment from a message. Script failures quote a stderr tail into the
// error, which the provider publishes as a NodeClaim event — and a join script
// running under `set -x` traces its exported bootstrap token and secrets there.
func (c *Context) Redact(msg string) string {
	values := make([]string, 0, len(c.Secrets)+1)
	if c.BootstrapToken != "" {
		values = append(values, c.BootstrapToken)
		// The secret half alone is enough to authenticate with a known id.
		if _, secret, ok := strings.Cut(c.BootstrapToken, "."); ok {
			values = append(values, secret)
		}
	}
	for _, v := range c.Secrets {
		values = append(values, v)
	}
	// Longest first: a value containing another must not be half-redacted.
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	for _, v := range values {
		if len(v) < minRedactableLen {
			continue
		}
		msg = strings.ReplaceAll(msg, v, redactionPlaceholder)
	}
	return msg
}

// Timeout resolves the execution deadline for a phase: the profile's
// spec.timeouts override when set and positive, the provider default
// otherwise.
func Timeout(p *infrav1.SSHJoinProfile, phase string, def time.Duration) time.Duration {
	t := p.Spec.Timeouts
	if t == nil {
		return def
	}
	var d *metav1.Duration
	switch phase {
	case PhaseInstall:
		d = t.Install
	case PhaseJoin:
		d = t.Join
	case PhaseLeave:
		d = t.Leave
	}
	if d == nil || d.Duration <= 0 {
		return def
	}
	return d.Duration
}

// Script returns the raw, unrendered script for a phase ("" when unset or
// unknown). Verified exec sends these bytes verbatim — they must match what
// was signed.
func Script(p *infrav1.SSHJoinProfile, phase string) string {
	switch phase {
	case PhaseInstall:
		return p.Spec.Scripts.Install
	case PhaseJoin:
		return p.Spec.Scripts.Join
	case PhaseLeave:
		return p.Spec.Scripts.Leave
	case PhaseUninstall:
		return p.Spec.Scripts.Uninstall
	}
	return ""
}

// Signature returns the armored SSHSIG for a phase ("" when unset).
func Signature(p *infrav1.SSHJoinProfile, phase string) string {
	s := p.Spec.Signatures
	if s == nil {
		return ""
	}
	switch phase {
	case PhaseInstall:
		return s.Install
	case PhaseJoin:
		return s.Join
	case PhaseLeave:
		return s.Leave
	case PhaseUninstall:
		return s.Uninstall
	}
	return ""
}

// HasTemplateActions reports whether raw contains Go text/template actions,
// which verified exec forbids: the executed bytes must equal the signed bytes,
// so all per-node variability must arrive as KPSSH_* params instead.
func HasTemplateActions(raw string) bool {
	return strings.Contains(raw, "{{")
}

func parse(phase, raw string) (*template.Template, error) {
	tpl, err := template.New(phase).Option("missingkey=error").Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing %s script: %w", phase, err)
	}
	return tpl, nil
}

// Render produces the executable script for a phase.
func Render(p *infrav1.SSHJoinProfile, phase string, ctx *Context) (string, error) {
	switch phase {
	case PhaseInstall, PhaseJoin, PhaseLeave, PhaseUninstall:
	default:
		return "", fmt.Errorf("unknown phase %q", phase)
	}
	raw := Script(p, phase)
	if raw == "" {
		return "", fmt.Errorf("profile %s has no %s script", p.Name, phase)
	}

	tpl, err := parse(phase, raw)
	if err != nil {
		return "", fmt.Errorf("profile %s: %w", p.Name, err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("rendering %s script of profile %s: %w", phase, p.Name, err)
	}
	return buf.String(), nil
}

// Validate reports why a profile cannot be used, before a NodeClaim depends on
// it: scripts must parse, and leave must render against an empty context.
//
// The leave check is not pedantry — the release path and the zombie guard both
// run leave without a node class in hand, so a leave script reaching for
// {{.Vars.x}} or {{.Secrets.y}} fails at exactly the moment a host must be
// disconnected, and the guard then retries it forever. The nodeclass Ready
// condition surfaces this instead.
func Validate(p *infrav1.SSHJoinProfile) error {
	for _, phase := range []string{PhaseInstall, PhaseJoin, PhaseLeave, PhaseUninstall} {
		raw := Script(p, phase)
		if raw == "" {
			if phase == PhaseUninstall {
				continue // optional
			}
			return fmt.Errorf("profile %s has no %s script", p.Name, phase)
		}
		if _, err := parse(phase, raw); err != nil {
			return err
		}
	}
	if _, err := Render(p, PhaseLeave, &Context{}); err != nil {
		return fmt.Errorf("leave script must render without a node class (it runs on release and zombie-guard paths): %w", err)
	}
	return nil
}

// InstalledMarker is the cache key stored in SSHHost.status.installedProfile.
func InstalledMarker(p *infrav1.SSHJoinProfile) string {
	v := p.Spec.Version
	if v == "" {
		v = "1"
	}
	return p.Name + "@" + v
}
