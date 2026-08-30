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

package host

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

const ns = "kpssh-system"

func newHost(name, class string, state v1beta1.HostState, created time.Time) *v1beta1.SSHHost {
	return &v1beta1.SSHHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			Labels:            map[string]string{v1beta1.HostClassLabel: class},
			CreationTimestamp: metav1.Time{Time: created},
		},
		Spec:   v1beta1.SSHHostSpec{Address: "10.0.0.1", SSHKeySecretRef: corev1.LocalObjectReference{Name: "k"}},
		Status: v1beta1.SSHHostStatus{State: state},
	}
}

func newProvider(t *testing.T, objs ...runtime.Object) *DefaultProvider {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme). // provider types registered via doc.go init()
		WithStatusSubresource(&v1beta1.SSHHost{}).
		WithRuntimeObjects(objs...).
		Build()
	return NewDefaultProvider(c, ns)
}

func TestProviderIDRoundTrip(t *testing.T) {
	h := newHost("host-a", "big", v1beta1.HostStateAvailable, time.Now())
	pid := ProviderID(h)
	if pid != "kpssh://kpssh-system/host-a" {
		t.Fatalf("ProviderID = %s", pid)
	}

	p := newProvider(t, h)
	got, err := p.ByProviderID(context.Background(), pid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "host-a" {
		t.Fatalf("resolved %s", got.Name)
	}
}

func TestByProviderIDMalformed(t *testing.T) {
	p := newProvider(t)
	if _, err := p.ByProviderID(context.Background(), "kpssh://no-slash"); err == nil {
		t.Fatal("expected error for malformed providerID")
	}
}

func TestByProviderIDAdopted(t *testing.T) {
	h := newHost("host-a", "big", v1beta1.HostStateInUse, time.Now())
	adopted := "eks-hybrid:///eu-central-1/cluster/mi-0abc"
	h.Status.ProviderID = adopted
	p := newProvider(t, h)

	got, err := p.ByProviderID(context.Background(), adopted)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "host-a" {
		t.Fatalf("resolved %s", got.Name)
	}

	_, err = p.ByProviderID(context.Background(), "eks-hybrid:///unknown")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestAvailableFiltersAndSorts(t *testing.T) {
	t0 := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	older := newHost("b-older", "big", v1beta1.HostStateAvailable, t0)
	newer := newHost("a-newer", "big", v1beta1.HostStateAvailable, t0.Add(time.Hour))
	claimed := newHost("claimed", "big", v1beta1.HostStateClaimed, t0)
	claimed.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc"}
	otherClass := newHost("small-1", "small", v1beta1.HostStateAvailable, t0)
	unhealthy := newHost("sick", "big", v1beta1.HostStateUnhealthy, t0)

	p := newProvider(t, older, newer, claimed, otherClass, unhealthy)
	got, err := p.Available(context.Background(), nil, "big")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "b-older" || got[1].Name != "a-newer" {
		names := []string{}
		for _, h := range got {
			names = append(names, h.Name)
		}
		t.Fatalf("Available = %v, want [b-older a-newer]", names)
	}
}

func TestReleaseHealthyAndUnhealthy(t *testing.T) {
	h := newHost("host-a", "big", v1beta1.HostStateInUse, time.Now())
	h.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc"}
	h.Status.ProviderID = "eks-hybrid:///eu-central-1/cluster/mi-0abc"
	p := newProvider(t, h)
	ctx := context.Background()

	if err := p.Release(ctx, h.DeepCopy(), true, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := p.ByProviderID(ctx, ProviderID(h))
	if got.Status.State != v1beta1.HostStateAvailable || got.Status.ClaimRef != nil {
		t.Fatalf("healthy release: state=%s claimRef=%v", got.Status.State, got.Status.ClaimRef)
	}
	if got.Status.ProviderID != "" {
		t.Fatalf("release must clear providerID, got %q", got.Status.ProviderID)
	}

	if err := p.Release(ctx, got.DeepCopy(), false, "join failed"); err != nil {
		t.Fatal(err)
	}
	got, _ = p.ByProviderID(ctx, ProviderID(h))
	if got.Status.State != v1beta1.HostStateUnhealthy || got.Status.LastProbeError != "join failed" {
		t.Fatalf("unhealthy release: state=%s err=%q", got.Status.State, got.Status.LastProbeError)
	}
}

// claimProvider builds a provider whose status-update path is intercepted,
// so Claim's CAS behavior can be driven deterministically.
func claimProvider(t *testing.T, h *v1beta1.SSHHost, update func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error) (*DefaultProvider, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithStatusSubresource(&v1beta1.SSHHost{}).
		WithRuntimeObjects(h).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: update}).
		Build()
	return NewDefaultProvider(c, ns), c
}

// TestClaimConflictRetry: losing the claim CAS is a normal lost race, not an
// error — Claim maps a 409 to (false, nil) and the caller picks another host
// (or retries). The retry against fresh state must succeed.
func TestClaimConflictRetry(t *testing.T) {
	h := newHost("host-a", "big", v1beta1.HostStateAvailable, time.Now())
	conflicted := false
	p, c := claimProvider(t, h, func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if !conflicted {
			conflicted = true
			return apierrors.NewConflict(
				v1beta1.GroupVersion.WithResource("sshhosts").GroupResource(),
				obj.GetName(), errors.New("simulated concurrent claim"))
		}
		return cl.SubResource(sub).Update(ctx, obj, opts...)
	})
	ctx := context.Background()
	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1", UID: "uid-1"}}

	ok, err := p.Claim(ctx, h.DeepCopy(), nc)
	if err != nil {
		t.Fatalf("conflict must map to (false, nil), got err: %v", err)
	}
	if ok {
		t.Fatal("conflict must map to ok=false")
	}

	// retry with a fresh read, as the instance provider does
	fresh := &v1beta1.SSHHost{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "host-a"}, fresh); err != nil {
		t.Fatal(err)
	}
	ok, err = p.Claim(ctx, fresh, nc)
	if err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if !ok {
		t.Fatal("retry claim must succeed once the conflict is gone")
	}

	got, err := p.ByProviderID(ctx, ProviderID(h))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.State != v1beta1.HostStateClaimed {
		t.Errorf("state = %s, want Claimed", got.Status.State)
	}
	if got.Status.ClaimRef == nil || got.Status.ClaimRef.Name != "nc-1" || got.Status.ClaimRef.UID != "uid-1" {
		t.Errorf("claimRef = %+v, want nc-1/uid-1", got.Status.ClaimRef)
	}
}

// TestClaimNonConflictErrorSurfaces: only 409s are swallowed; any other API
// error must reach the caller.
func TestClaimNonConflictErrorSurfaces(t *testing.T) {
	h := newHost("host-a", "big", v1beta1.HostStateAvailable, time.Now())
	p, _ := claimProvider(t, h, func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
		return apierrors.NewInternalError(errors.New("etcd down"))
	})
	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1", UID: "uid-1"}}

	ok, err := p.Claim(context.Background(), h.DeepCopy(), nc)
	if ok {
		t.Fatal("claim must not report success on an API error")
	}
	if err == nil {
		t.Fatal("non-conflict API error must surface, got nil")
	}
	if !apierrors.IsInternalError(err) {
		t.Fatalf("error must pass through unchanged, got: %v", err)
	}
}

// TestSetStatusRetriesLostCAS: the probe controller
// legitimately patches a claimed host's status while Create() is still writing
// (bootstrap-token collection), so the narrow field setters must survive a
// lost CAS by re-reading and re-applying — not fail a launch whose join
// already succeeded.
func TestSetStatusRetriesLostCAS(t *testing.T) {
	h := newHost("host-a", "big", v1beta1.HostStateClaimed, time.Now())
	h.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc-1", UID: "uid-1"}
	conflicted := false
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithStatusSubresource(&v1beta1.SSHHost{}).
		WithRuntimeObjects(h).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, cl client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if !conflicted {
					conflicted = true
					return apierrors.NewConflict(
						v1beta1.GroupVersion.WithResource("sshhosts").GroupResource(),
						obj.GetName(), errors.New("simulated concurrent token collection"))
				}
				return cl.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	p := NewDefaultProvider(c, ns)

	if err := p.SetProviderID(context.Background(), h, "kpssh://kpssh-system/host-a"); err != nil {
		t.Fatalf("SetProviderID must survive one lost CAS: %v", err)
	}
	got := &v1beta1.SSHHost{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "host-a"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ProviderID != "kpssh://kpssh-system/host-a" {
		t.Errorf("providerID = %q, want it written on retry", got.Status.ProviderID)
	}
}

// TestSetStatusFencesClaimChange: the retry must not resurrect writes for a
// claim that has since been released — a real conflict surfaces.
func TestSetStatusFencesClaimChange(t *testing.T) {
	stored := newHost("host-a", "big", v1beta1.HostStateClaimed, time.Now())
	stored.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc-new", UID: "uid-new"}
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithStatusSubresource(&v1beta1.SSHHost{}).
		WithRuntimeObjects(stored).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				return apierrors.NewConflict(
					v1beta1.GroupVersion.WithResource("sshhosts").GroupResource(),
					obj.GetName(), errors.New("simulated"))
			},
		}).
		Build()
	p := NewDefaultProvider(c, ns)

	mine := stored.DeepCopy()
	mine.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc-old", UID: "uid-old"}
	err := p.SetProviderID(context.Background(), mine, "kpssh://x/y")
	if err == nil {
		t.Fatal("claim changed hands — SetProviderID must fail, not retry blindly")
	}
	if apierrors.IsConflict(err) {
		t.Fatalf("fenced failure must not read as a retryable conflict: %v", err)
	}
	got := &v1beta1.SSHHost{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "host-a"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ProviderID != "" {
		t.Errorf("providerID written despite a foreign claim: %q", got.Status.ProviderID)
	}
}

// TestReleaseIsCompareAndSwap is the successor-stomping guard. The host state
// machine is shared between the claim path, the release path and the probe
// controller; a Release built from a stale read must fail rather than
// last-write-wins its way over a claim that has since been placed. (Delete
// runs for minutes — leave over SSH — so its snapshot really can go stale.)
func TestReleaseIsCompareAndSwap(t *testing.T) {
	p := newProvider(t, newHost("h", "big", v1beta1.HostStateInUse, time.Now()))

	stale, err := p.ByProviderID(context.Background(), "kpssh://"+ns+"/h")
	if err != nil {
		t.Fatal(err)
	}
	stale.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc-old", UID: "uid-old"}

	// Someone else moves the host on: the object we hold is now behind.
	fresh, err := p.ByProviderID(context.Background(), "kpssh://"+ns+"/h")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := p.Claim(context.Background(), fresh, &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "nc-new", UID: "uid-new"},
	}); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}

	err = p.Release(context.Background(), stale, true, "")
	if !apierrors.IsConflict(err) {
		t.Fatalf("stale Release must conflict, got %v", err)
	}
	got := &v1beta1.SSHHost{}
	if err := p.kubeClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "h"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ClaimRef == nil || got.Status.ClaimRef.Name != "nc-new" {
		t.Fatalf("live claim was stomped by a stale release: %v", got.Status.ClaimRef)
	}
}

func TestClaimHeldBy(t *testing.T) {
	h := newHost("h", "big", v1beta1.HostStateInUse, time.Now())
	h.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc-1", UID: "uid-1"}

	claim := func(name string, uid types.UID) *karpv1.NodeClaim {
		return &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
	}
	for _, tc := range []struct {
		name string
		nc   *karpv1.NodeClaim
		want bool
	}{
		{"same name and uid", claim("nc-1", "uid-1"), true},
		// Garbage collection reconstructs NodeClaims from cloudprovider.List():
		// name, no uid. Those must still match, or GC could never release.
		{"same name, no uid (garbage collection)", claim("nc-1", ""), true},
		{"same name, different uid", claim("nc-1", "uid-2"), false},
		{"different name", claim("nc-2", "uid-1"), false},
	} {
		if got := ClaimHeldBy(h, tc.nc); got != tc.want {
			t.Errorf("%s: ClaimHeldBy = %v, want %v", tc.name, got, tc.want)
		}
	}
	h.Status.ClaimRef = nil
	if ClaimHeldBy(h, claim("nc-1", "uid-1")) {
		t.Error("an unclaimed host is held by nobody")
	}
}

// An empty providerID must resolve to nothing. Without the guard it falls
// through to the adopted-id scan and matches the first host whose
// status.ProviderID was never set — handing back an arbitrary host that
// Delete would then leave.
func TestByProviderIDEmptyIsNotFound(t *testing.T) {
	// Two hosts, neither with a providerID — exactly the state the fall-through
	// would match.
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithRuntimeObjects(
			&v1beta1.SSHHost{ObjectMeta: metav1.ObjectMeta{Name: "host-a", Namespace: ns}},
			&v1beta1.SSHHost{ObjectMeta: metav1.ObjectMeta{Name: "host-b", Namespace: ns}},
		).Build()
	p := NewDefaultProvider(c, ns)

	got, err := p.ByProviderID(context.Background(), "")
	if err == nil {
		t.Fatalf("empty providerID resolved to host %q", got.Name)
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("err = %v, want NotFound (callers branch on it)", err)
	}
}

// ByClaim is Create's resume path, so a match here means "adopt this host's
// existing claim without re-Claiming it". Matching a dead predecessor that
// happened to share a name would resume its claim and leave the stale UID in
// ClaimRef — and the zombie guard, seeing ref.UID != nc.UID, would then
// force-leave the host out from under the live NodeClaim. Hence the fence.
func TestByClaimFencesOnUID(t *testing.T) {
	claimed := func(host, claimName string, uid types.UID) *v1beta1.SSHHost {
		h := newHost(host, "big", v1beta1.HostStateInUse, time.Now())
		h.Status.ClaimRef = &v1beta1.ClaimReference{Name: claimName, UID: uid}
		return h
	}
	nc := func(name string, uid types.UID) *karpv1.NodeClaim {
		return &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
	}

	for _, tc := range []struct {
		name  string
		host  *v1beta1.SSHHost
		claim *karpv1.NodeClaim
		want  string // host name, or "" for no match
	}{
		{"same name and UID", claimed("host-a", "nc-1", "uid-1"), nc("nc-1", "uid-1"), "host-a"},
		{"name reused by a new object", claimed("host-a", "nc-1", "uid-old"), nc("nc-1", "uid-new"), ""},
		{"different name", claimed("host-a", "nc-2", "uid-1"), nc("nc-1", "uid-1"), ""},
		// Pre-UID claims must stay adoptable, not get stranded — same
		// concession the zombie guard makes in hostprobe.go.
		{"claim predates the UID field", claimed("host-a", "nc-1", ""), nc("nc-1", "uid-1"), "host-a"},
		{"NodeClaim has no UID", claimed("host-a", "nc-1", "uid-1"), nc("nc-1", ""), "host-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newProvider(t, tc.host).ByClaim(context.Background(), tc.claim)
			if err != nil {
				t.Fatalf("ByClaim: %v", err)
			}
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("matched %s; a stale claim must not be resumed", got.Name)
			case tc.want != "" && got == nil:
				t.Fatalf("no match; want %s", tc.want)
			case tc.want != "" && got.Name != tc.want:
				t.Fatalf("matched %s; want %s", got.Name, tc.want)
			}
		})
	}
}
