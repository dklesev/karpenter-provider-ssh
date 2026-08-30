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

package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/dklesev/karpenter-provider-ssh/internal/sshexec"
	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/bootstrap"
)

const testNS = "kpssh-system"

func probeHost(name string, state v1beta1.HostState) *v1beta1.SSHHost {
	return &v1beta1.SSHHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  testNS,
			Generation: 1,
		},
		Spec: v1beta1.SSHHostSpec{
			Address:         "10.0.0.1",
			SSHKeySecretRef: corev1.LocalObjectReference{Name: "k"},
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("8"),
				corev1.ResourceMemory: resource.MustParse("62Gi"),
			},
		},
		Status: v1beta1.SSHHostStatus{State: state},
	}
}

func sshKeySecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: testNS},
		Data:       map[string][]byte{"privateKey": []byte("dummy-pem")},
	}
}

func newReconciler(t *testing.T, exec sshexec.Runner, objs ...runtime.Object) (*HostProbeReconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithStatusSubresource(&v1beta1.SSHHost{}).
		WithRuntimeObjects(objs...).
		// The zombie guard resolves a host to its Node through the
		// InternalIP field index that pkg/operator registers on the real
		// manager. Same index function, so the two cannot drift.
		WithIndex(&corev1.Node{}, v1beta1.NodeInternalIPIndex,
			func(o client.Object) []string { return v1beta1.NodeInternalIPs(o.(*corev1.Node)) }).
		Build()
	return &HostProbeReconciler{
		Client:              c,
		KubernetesInterface: k8sfake.NewSimpleClientset(sshKeySecret()),
		Exec:                exec,
		Bootstrap:           &fakeTokens{},
	}, c
}

// fakeTokens records bootstrap-token deletions.
type fakeTokens struct {
	bootstrap.Provider
	deleted []string
}

func (f *fakeTokens) DeleteTokenByID(_ context.Context, tokenID string) error {
	f.deleted = append(f.deleted, tokenID)
	return nil
}

func execReturning(stdout string) sshexec.Runner {
	return func(context.Context, sshexec.Target, string, map[string]string) (*sshexec.Result, error) {
		return &sshexec.Result{Stdout: stdout, HostKeyFingerprint: "SHA256:probed"}, nil
	}
}

func reconcileHost(t *testing.T, r *HostProbeReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNS, Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func getHost(t *testing.T, c client.Client, name string) *v1beta1.SSHHost {
	t.Helper()
	h := &v1beta1.SSHHost{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestProbeSuccessMarksAvailable(t *testing.T) {
	r, c := newReconciler(t, execReturning("8\n65011712\nx86_64\ninactive\n"), probeHost("h", v1beta1.HostStatePending))

	res := reconcileHost(t, r, "h")
	if res.RequeueAfter != ProbeInterval {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, ProbeInterval)
	}
	h := getHost(t, c, "h")
	if h.Status.State != v1beta1.HostStateAvailable {
		t.Fatalf("state = %s", h.Status.State)
	}
	if h.Status.HostKeyFingerprint != "SHA256:probed" {
		t.Errorf("fingerprint not TOFU-pinned: %q", h.Status.HostKeyFingerprint)
	}
	if got := h.Status.ObservedCapacity.Cpu().Value(); got != 8 {
		t.Errorf("observed cpu = %d", got)
	}
	if h.Status.ObservedArch != "amd64" {
		t.Errorf("observed arch = %s", h.Status.ObservedArch)
	}
	if h.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d", h.Status.ObservedGeneration)
	}
}

// TestStalenessGuardSkipsFreshProbe guards against the self-triggering hot
// loop: a reconcile caused by our own status patch must NOT probe again
// before ProbeInterval has passed.
func TestStalenessGuardSkipsFreshProbe(t *testing.T) {
	h := probeHost("h", v1beta1.HostStateAvailable)
	now := metav1.Now()
	h.Status.LastProbeTime = &now
	h.Status.ObservedGeneration = 1

	execCalled := false
	exec := func(context.Context, sshexec.Target, string, map[string]string) (*sshexec.Result, error) {
		execCalled = true
		return &sshexec.Result{}, nil
	}
	r, _ := newReconciler(t, exec, h)

	res := reconcileHost(t, r, "h")
	if execCalled {
		t.Fatal("probe ran despite fresh LastProbeTime — self-triggering hot loop")
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > ProbeInterval {
		t.Fatalf("RequeueAfter = %v, want (0, %v]", res.RequeueAfter, ProbeInterval)
	}
}

func TestStalenessGuardBypassedOnSpecChange(t *testing.T) {
	h := probeHost("h", v1beta1.HostStateAvailable)
	h.Generation = 2 // spec changed since last probe
	now := metav1.Now()
	h.Status.LastProbeTime = &now
	h.Status.ObservedGeneration = 1

	r, c := newReconciler(t, execReturning("8\n65011712\nx86_64\ninactive\n"), h)
	reconcileHost(t, r, "h")
	if got := getHost(t, c, "h").Status.ObservedGeneration; got != 2 {
		t.Fatalf("observedGeneration = %d, want 2 (probe must run on spec change)", got)
	}
}

func TestProbeFailureMarksUnhealthy(t *testing.T) {
	exec := func(context.Context, sshexec.Target, string, map[string]string) (*sshexec.Result, error) {
		return nil, fmt.Errorf("connection refused")
	}
	r, c := newReconciler(t, exec, probeHost("h", v1beta1.HostStatePending))

	reconcileHost(t, r, "h")
	h := getHost(t, c, "h")
	if h.Status.State != v1beta1.HostStateUnhealthy {
		t.Fatalf("state = %s", h.Status.State)
	}
	if !strings.Contains(h.Status.LastProbeError, "connection refused") {
		t.Errorf("lastProbeError = %q", h.Status.LastProbeError)
	}
}

func TestStaleClaimReleased(t *testing.T) {
	h := probeHost("h", v1beta1.HostStateInUse)
	h.Status.ClaimRef = &v1beta1.ClaimReference{Name: "gone"}
	r, c := newReconciler(t, execReturning(""), h)

	reconcileHost(t, r, "h")
	got := getHost(t, c, "h")
	if got.Status.ClaimRef != nil || got.Status.State != v1beta1.HostStatePending {
		t.Fatalf("stale claim not released: state=%s claimRef=%v", got.Status.State, got.Status.ClaimRef)
	}
}

func TestMaintenanceTransitions(t *testing.T) {
	h := probeHost("h", v1beta1.HostStateAvailable)
	h.Annotations = map[string]string{v1beta1.MaintenanceAnnotation: "true"}
	r, c := newReconciler(t, execReturning(""), h)

	reconcileHost(t, r, "h")
	if got := getHost(t, c, "h"); got.Status.State != v1beta1.HostStateMaintenance {
		t.Fatalf("state = %s, want Maintenance", got.Status.State)
	}

	// annotation removed → back to Pending (then probed on the next cycle)
	got := getHost(t, c, "h")
	got.Annotations = nil
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileHost(t, r, "h")
	if got := getHost(t, c, "h"); got.Status.State == v1beta1.HostStateMaintenance {
		t.Fatalf("state = %s, must leave Maintenance", got.Status.State)
	}
}

// TestForeignKubeletParksHost: an active kubelet on a host this provider never
// installed must park the host, never run leave.
func TestForeignKubeletParksHost(t *testing.T) {
	r, c := newReconciler(t, execReturning("4\n16384000\nx86_64\nactive\n"), probeHost("h", v1beta1.HostStatePending))

	reconcileHost(t, r, "h")
	h := getHost(t, c, "h")
	if h.Status.State != v1beta1.HostStateUnhealthy {
		t.Fatalf("state = %s, want Unhealthy (parked)", h.Status.State)
	}
	if !strings.Contains(h.Status.LastProbeError, "foreign membership") {
		t.Errorf("lastProbeError = %q", h.Status.LastProbeError)
	}
}

// TestZombieGuardSkipsClaimedHost: a claim landing while the probe's SSH
// session is in flight (injected via the exec hook) bumps the resourceVersion,
// so the guard's optimistic patches must lose and back off — the claimed host
// is never fenced or left.
func TestZombieGuardSkipsClaimedHost(t *testing.T) {
	h := probeHost("h", v1beta1.HostStatePending)
	h.Status.InstalledProfile = "prof@1"

	var cl client.Client
	exec := func(context.Context, sshexec.Target, string, map[string]string) (*sshexec.Result, error) {
		cur := &v1beta1.SSHHost{}
		if err := cl.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: "h"}, cur); err != nil {
			return nil, err
		}
		cur.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc-race"}
		if err := cl.Status().Update(context.Background(), cur); err != nil {
			return nil, err
		}
		return &sshexec.Result{Stdout: "4\n16384000\nx86_64\nactive\n"}, nil
	}
	r, c := newReconciler(t, exec, h)
	cl = c

	res := reconcileHost(t, r, "h")
	got := getHost(t, c, "h")
	if got.Status.State == v1beta1.HostStateLeaving {
		t.Fatalf("guard must not fence a claimed host, state = %s", got.Status.State)
	}
	if got.Status.ClaimRef == nil {
		t.Fatal("claimRef lost")
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected requeue")
	}
}

func TestCapacityShortfall(t *testing.T) {
	spec := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("8"),
		corev1.ResourceMemory: resource.MustParse("64Gi"),
	}
	if s := capacityShortfall(spec, nil); len(s) != 0 {
		t.Errorf("nil observed must not report drift: %v", s)
	}
	ok := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("8"),
		corev1.ResourceMemory: resource.MustParse("64Gi"),
	}
	if s := capacityShortfall(spec, ok); len(s) != 0 {
		t.Errorf("equal capacity must not report drift: %v", s)
	}
	short := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("64Gi"),
	}
	s := capacityShortfall(spec, short)
	if len(s) != 1 || !strings.Contains(s[0], "cpu") {
		t.Errorf("want cpu shortfall, got %v", s)
	}
}

func TestParseProbeOutput(t *testing.T) {
	cases := []struct {
		name          string
		stdout        string
		cpus, memKB   int64
		arch          string
		kubeletActive bool
	}{
		{
			name:   "idle host",
			stdout: "8\n65536000\nx86_64\ninactive\n",
			cpus:   8, memKB: 65536000, arch: "amd64", kubeletActive: false,
		},
		{
			name:   "zombie kubelet",
			stdout: "4\n16384000\naarch64\nactive\n",
			cpus:   4, memKB: 16384000, arch: "arm64", kubeletActive: true,
		},
		{
			name:   "kubelet unit missing (is-active prints unknown)",
			stdout: "2\n4096000\nx86_64\nunknown\n",
			cpus:   2, memKB: 4096000, arch: "amd64", kubeletActive: false,
		},
		{
			name:   "three fields only (no kubelet line)",
			stdout: "2\n4096000\nx86_64\n",
			cpus:   2, memKB: 4096000, arch: "amd64", kubeletActive: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := parseProbeOutput(tc.stdout, "SHA256:x")
			if res.cpus != tc.cpus || res.memKB != tc.memKB || res.arch != tc.arch || res.kubeletActive != tc.kubeletActive {
				t.Fatalf("got %+v", res)
			}
			if res.fingerprint != "SHA256:x" {
				t.Fatalf("fingerprint lost: %+v", res)
			}
		})
	}
}

func TestNormalizeArch(t *testing.T) {
	for in, want := range map[string]string{
		"x86_64":  "amd64",
		"aarch64": "arm64",
		"arm64":   "arm64",
		"riscv64": "riscv64",
	} {
		if got := normalizeArch(in); got != want {
			t.Errorf("normalizeArch(%s) = %s, want %s", in, got, want)
		}
	}
}

// The bootstrap token dies with the node's registration — by then the kubelet
// holds a client certificate and will never present the token again. Nothing
// else collects it: kube-controller-manager's tokencleaner is disabled by
// default upstream, so an uncollected Secret outlives its TTL in kube-system
// forever, one per launch.
func TestClaimedHostCollectsBootstrapTokenOnceNodeRegisters(t *testing.T) {
	h := probeHost("host-a", v1beta1.HostStateInUse)
	h.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc-1", UID: "uid-1"}
	h.Status.BootstrapTokenID = "abcdef"
	h.Status.ProviderID = "kpssh://kpssh-system/host-a"
	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1", UID: "uid-1"}}

	r, c := newReconciler(t, execReturning(""), h, nc)
	tokens := r.Bootstrap.(*fakeTokens)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: "host-a"}}

	// No Node advertising the host's IP yet: the kubelet may still be
	// bootstrapping, so the token must survive.
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens.deleted) != 0 {
		t.Fatalf("token deleted before the node registered: %v", tokens.deleted)
	}
	if res.RequeueAfter != tokenRecheck {
		t.Errorf("requeue = %s, want the short token recheck %s", res.RequeueAfter, tokenRecheck)
	}
	if got := getHost(t, c, "host-a"); got.Status.BootstrapTokenID != "abcdef" {
		t.Errorf("bootstrapTokenID = %q, want it kept", got.Status.BootstrapTokenID)
	}

	// The node registers with the host's InternalIP → the token is spent.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "nc-1"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
		}},
	}
	if err := c.Create(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(tokens.deleted) != 1 || tokens.deleted[0] != "abcdef" {
		t.Fatalf("deleted = %v, want the host's token", tokens.deleted)
	}
	if got := getHost(t, c, "host-a"); got.Status.BootstrapTokenID != "" {
		t.Errorf("bootstrapTokenID = %q, want cleared", got.Status.BootstrapTokenID)
	}
}

// While Create() is still writing (no providerID recorded yet), the token must
// survive even when the Node is already registered — a pre-registered Node is
// exactly the adopt/hand-over case, and collecting now would patch the status
// mid-Create and steal its optimistic lock.
func TestClaimedHostKeepsTokenWhileLaunchInFlight(t *testing.T) {
	h := probeHost("host-a", v1beta1.HostStateClaimed)
	h.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc-1", UID: "uid-1"}
	h.Status.BootstrapTokenID = "abcdef" // ProviderID deliberately empty
	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1", UID: "uid-1"}}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "pre-registered"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
		}},
	}

	r, c := newReconciler(t, execReturning(""), h, nc, node)
	tokens := r.Bootstrap.(*fakeTokens)
	reconcileHost(t, r, "host-a")
	if len(tokens.deleted) != 0 {
		t.Fatalf("token collected mid-launch: %v", tokens.deleted)
	}
	if got := getHost(t, c, "host-a"); got.Status.BootstrapTokenID != "abcdef" {
		t.Errorf("bootstrapTokenID = %q, want it kept until Create records the providerID", got.Status.BootstrapTokenID)
	}
}

// A NodeClaim whose name was reused by a different object does not hold this
// host any more — the UID is what tells them apart.
func TestStaleClaimReleasedOnUIDMismatch(t *testing.T) {
	h := probeHost("host-a", v1beta1.HostStateInUse)
	h.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc-1", UID: "uid-old"}
	h.Status.BootstrapTokenID = "abcdef"
	h.Status.ProviderID = "kpssh://kpssh-system/host-a"
	reused := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1", UID: "uid-new"}}

	r, c := newReconciler(t, execReturning(""), h, reused)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNS, Name: "host-a"},
	}); err != nil {
		t.Fatal(err)
	}

	got := getHost(t, c, "host-a")
	if got.Status.ClaimRef != nil || got.Status.ProviderID != "" {
		t.Errorf("stale claim not released: claimRef=%v providerID=%q", got.Status.ClaimRef, got.Status.ProviderID)
	}
	if got.Status.State != v1beta1.HostStatePending {
		t.Errorf("state = %s, want Pending (re-probe before it can be claimed)", got.Status.State)
	}
	if tokens := r.Bootstrap.(*fakeTokens); len(tokens.deleted) != 1 {
		t.Errorf("the dead claim's token must be collected: %v", tokens.deleted)
	}
}
