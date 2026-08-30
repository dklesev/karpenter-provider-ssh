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

// Package host manages the SSHHost pool: listing, claiming (CAS on
// status.claimRef), releasing, and resolving hosts from providerIDs.
package host

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

// Provider is the host pool interface.
//
// Every status write is a compare-and-swap: the host state machine is shared
// between the claim path, the release path and the probe controller, so a
// last-write-wins merge could resurrect a state another actor just left (a
// late Release from a dead NodeClaim stomping a live claim, say). Claim and
// Release surface lost races to their callers, who re-resolve from scratch.
// The narrower field setters (providerID, install marker, token id) retry a
// lost race on a fresh read instead — but only while the claim they were
// issued under still holds; see setStatus.
type Provider interface {
	// Available lists claimable hosts matching the selector, oldest first,
	// optionally restricted to one host class.
	Available(ctx context.Context, selector *metav1.LabelSelector, class string) ([]*v1beta1.SSHHost, error)
	// All lists every host matching the selector.
	All(ctx context.Context, selector *metav1.LabelSelector) ([]*v1beta1.SSHHost, error)
	// Claim atomically locks a host for a NodeClaim. Returns false on lost race.
	Claim(ctx context.Context, host *v1beta1.SSHHost, nodeClaim *karpv1.NodeClaim) (bool, error)
	// Release clears the claim, the providerID and the bootstrap-token record
	// in one patch. healthy=false parks the host Unhealthy until re-probed.
	Release(ctx context.Context, host *v1beta1.SSHHost, healthy bool, reason string) error
	// MarkInUse transitions a claimed host once its node is up.
	MarkInUse(ctx context.Context, host *v1beta1.SSHHost) error
	// SetInstalled records the install-cache marker.
	SetInstalled(ctx context.Context, host *v1beta1.SSHHost, marker string) error
	// ByProviderID resolves kpssh://<namespace>/<name> or an adopted external id.
	ByProviderID(ctx context.Context, providerID string) (*v1beta1.SSHHost, error)
	// SetProviderID records the providerID the host currently backs ("" clears).
	SetProviderID(ctx context.Context, host *v1beta1.SSHHost, providerID string) error
	// SetBootstrapTokenID records the id of the token minted for the in-flight
	// join, so it can be deleted once the node registers ("" clears).
	SetBootstrapTokenID(ctx context.Context, host *v1beta1.SSHHost, tokenID string) error
	// ByClaim finds the host locked by the given NodeClaim, if any. Fenced on
	// UID as well as name — see the implementation.
	ByClaim(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*v1beta1.SSHHost, error)
	// Claimed lists all hosts with a claim.
	Claimed(ctx context.Context) ([]*v1beta1.SSHHost, error)
}

// ClaimHeldBy reports whether the host's claim belongs to this NodeClaim.
//
// providerIDs are host-scoped in static mode (kpssh://<ns>/<name> never
// changes), and karpenter core retries Delete until it reports the claim gone —
// so a Delete for a long-dead NodeClaim can still resolve a host that a
// *successor* claim has since taken. Without this fence that Delete would run
// leave against the successor's freshly joined node. The UID is compared only
// when both sides carry one: NodeClaims reconstructed by garbage collection
// from cloudprovider.List() have a name but no UID.
func ClaimHeldBy(h *v1beta1.SSHHost, nodeClaim *karpv1.NodeClaim) bool {
	ref := h.Status.ClaimRef
	if ref == nil || ref.Name != nodeClaim.Name {
		return false
	}
	if ref.UID != "" && nodeClaim.UID != "" && ref.UID != nodeClaim.UID {
		return false
	}
	return true
}

// DefaultProvider implements Provider against the management API.
type DefaultProvider struct {
	kubeClient client.Client
	namespace  string
}

// NewDefaultProvider returns a pool provider scoped to one namespace.
func NewDefaultProvider(kubeClient client.Client, namespace string) *DefaultProvider {
	return &DefaultProvider{kubeClient: kubeClient, namespace: namespace}
}

func (p *DefaultProvider) list(ctx context.Context, selector *metav1.LabelSelector) ([]*v1beta1.SSHHost, error) {
	opts := []client.ListOption{client.InNamespace(p.namespace)}
	if selector != nil {
		sel, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return nil, fmt.Errorf("invalid host selector: %w", err)
		}
		opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
	}
	hosts := &v1beta1.SSHHostList{}
	if err := p.kubeClient.List(ctx, hosts, opts...); err != nil {
		return nil, err
	}
	out := make([]*v1beta1.SSHHost, 0, len(hosts.Items))
	for i := range hosts.Items {
		out = append(out, &hosts.Items[i])
	}
	return out, nil
}

// All implements Provider.
func (p *DefaultProvider) All(ctx context.Context, selector *metav1.LabelSelector) ([]*v1beta1.SSHHost, error) {
	return p.list(ctx, selector)
}

// Available implements Provider.
func (p *DefaultProvider) Available(ctx context.Context, selector *metav1.LabelSelector, class string) ([]*v1beta1.SSHHost, error) {
	hosts, err := p.list(ctx, selector)
	if err != nil {
		return nil, err
	}
	out := []*v1beta1.SSHHost{}
	for _, h := range hosts {
		if h.Status.State != v1beta1.HostStateAvailable || h.Status.ClaimRef != nil {
			continue
		}
		// A just-released host can still read Available for a probe cycle
		// after being annotated for maintenance — honor the annotation here
		// so it is never claimable in that window.
		if _, maint := h.Annotations[v1beta1.MaintenanceAnnotation]; maint {
			continue
		}
		if class != "" && v1beta1.HostClass(h) != class {
			continue
		}
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b *v1beta1.SSHHost) int {
		if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// Claim implements Provider.
func (p *DefaultProvider) Claim(ctx context.Context, host *v1beta1.SSHHost, nodeClaim *karpv1.NodeClaim) (bool, error) {
	host.Status.State = v1beta1.HostStateClaimed
	host.Status.ClaimRef = &v1beta1.ClaimReference{Name: nodeClaim.Name, UID: nodeClaim.UID}
	if err := p.kubeClient.Status().Update(ctx, host); err != nil {
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// casPatch applies a status patch as a compare-and-swap against the host's
// resourceVersion. The host object is updated in place with the server's
// response — never re-read it from the cache to build the next patch, the
// informer will not have ingested this write yet.
func (p *DefaultProvider) casPatch(ctx context.Context, host, orig *v1beta1.SSHHost) error {
	return p.kubeClient.Status().Patch(ctx, host,
		client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

// setStatus applies mutate to the host's status, retrying lost CAS races on a
// fresh read. The fields written this way (providerID, install marker, token
// id) are last-writer-wins between actors that hold the claim, and the probe
// controller legitimately patches the same status mid-Create (bootstrap-token
// collection) — without the retry, one such patch fails the whole launch after
// a join that succeeded. The retry is fenced: if the fresh read shows the
// claim gone or held by someone else, the host has moved on and the conflict
// is surfaced for the caller to re-resolve. host is updated in place so
// subsequent writes chain off the latest state.
func (p *DefaultProvider) setStatus(ctx context.Context, host *v1beta1.SSHHost, mutate func(*v1beta1.SSHHost)) error {
	expect := host.Status.ClaimRef
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		orig := host.DeepCopy()
		mutate(host)
		err := p.casPatch(ctx, host, orig)
		if err == nil || !apierrors.IsConflict(err) {
			return err
		}
		fresh := &v1beta1.SSHHost{}
		if gerr := p.kubeClient.Get(ctx, client.ObjectKeyFromObject(host), fresh); gerr != nil {
			return gerr
		}
		if !sameClaim(expect, fresh.Status.ClaimRef) {
			return fmt.Errorf("host %s claim changed while writing status (now %v)", host.Name, fresh.Status.ClaimRef)
		}
		*host = *fresh
		return err
	})
}

// sameClaim reports whether two claim references point at the same NodeClaim.
func sameClaim(a, b *v1beta1.ClaimReference) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Name == b.Name && a.UID == b.UID
}

// Release implements Provider.
func (p *DefaultProvider) Release(ctx context.Context, host *v1beta1.SSHHost, healthy bool, reason string) error {
	orig := host.DeepCopy()
	host.Status.ClaimRef = nil
	host.Status.ProviderID = ""
	host.Status.BootstrapTokenID = ""
	if healthy {
		host.Status.State = v1beta1.HostStateAvailable
	} else {
		host.Status.State = v1beta1.HostStateUnhealthy
		host.Status.LastProbeError = reason
	}
	return p.casPatch(ctx, host, orig)
}

// MarkInUse implements Provider.
func (p *DefaultProvider) MarkInUse(ctx context.Context, host *v1beta1.SSHHost) error {
	return p.setStatus(ctx, host, func(h *v1beta1.SSHHost) { h.Status.State = v1beta1.HostStateInUse })
}

// SetInstalled implements Provider.
func (p *DefaultProvider) SetInstalled(ctx context.Context, host *v1beta1.SSHHost, marker string) error {
	return p.setStatus(ctx, host, func(h *v1beta1.SSHHost) { h.Status.InstalledProfile = marker })
}

// SetProviderID implements Provider.
func (p *DefaultProvider) SetProviderID(ctx context.Context, host *v1beta1.SSHHost, providerID string) error {
	return p.setStatus(ctx, host, func(h *v1beta1.SSHHost) { h.Status.ProviderID = providerID })
}

// SetBootstrapTokenID implements Provider.
func (p *DefaultProvider) SetBootstrapTokenID(ctx context.Context, host *v1beta1.SSHHost, tokenID string) error {
	return p.setStatus(ctx, host, func(h *v1beta1.SSHHost) { h.Status.BootstrapTokenID = tokenID })
}

// ProviderID renders the providerID for a host.
func ProviderID(host *v1beta1.SSHHost) string {
	return fmt.Sprintf("%s%s/%s", v1beta1.ProviderPrefix, host.Namespace, host.Name)
}

// ByProviderID implements Provider. kpssh:// ids resolve directly; adopted
// external ids (e.g. eks-hybrid:///…) resolve via SSHHost.status.providerID.
func (p *DefaultProvider) ByProviderID(ctx context.Context, providerID string) (*v1beta1.SSHHost, error) {
	// An empty id must never resolve. Without this it falls through to the
	// adopted-id branch below and matches the first host whose ProviderID has
	// never been set — i.e. "" would hand back an arbitrary unclaimed host,
	// and a Delete on it would leave somebody else's node. Callers do guard,
	// but the guard belongs where the danger is.
	if providerID == "" {
		return nil, apierrors.NewNotFound(
			v1beta1.GroupVersion.WithResource("sshhosts").GroupResource(), "<empty providerID>")
	}
	if rest, ok := strings.CutPrefix(providerID, v1beta1.ProviderPrefix); ok {
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed providerID %q", providerID)
		}
		host := &v1beta1.SSHHost{}
		if err := p.kubeClient.Get(ctx, types.NamespacedName{Namespace: parts[0], Name: parts[1]}, host); err != nil {
			return nil, err
		}
		return host, nil
	}
	hosts, err := p.list(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, h := range hosts {
		if h.Status.ProviderID == providerID {
			return h, nil
		}
	}
	return nil, apierrors.NewNotFound(v1beta1.GroupVersion.WithResource("sshhosts").GroupResource(), providerID)
}

// ByClaim implements Provider.
//
// Matches on name AND UID, which is the fence ClaimReference documents. Name
// alone is not enough: karpenter mints NodeClaims with a generateName suffix,
// so a name recurs eventually, and this is Create's *resume* path. Matching a
// dead predecessor's leftover claim would resume it — skipping Claim(), which
// is the only thing that rewrites ClaimRef — and leave the stale UID in place.
// The zombie guard would then see ref.UID != nc.UID, correctly call it stale,
// and force-leave the host out from under the live NodeClaim it just joined.
//
// An empty UID on either side disables the fence, exactly as the zombie guard
// does: hosts claimed before the field existed must stay adoptable rather than
// be stranded.
func (p *DefaultProvider) ByClaim(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*v1beta1.SSHHost, error) {
	hosts, err := p.list(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, h := range hosts {
		if ClaimHeldBy(h, nodeClaim) {
			return h, nil
		}
	}
	return nil, nil
}

// Claimed implements Provider.
func (p *DefaultProvider) Claimed(ctx context.Context) ([]*v1beta1.SSHHost, error) {
	hosts, err := p.list(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := []*v1beta1.SSHHost{}
	for _, h := range hosts {
		if h.Status.ClaimRef != nil {
			out = append(out, h)
		}
	}
	return out, nil
}
