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
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/dklesev/karpenter-provider-ssh/internal/sshexec"
	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/bootstrap"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/host"
)

const testNS = "kpssh-system"

// fakeExec records executed scripts; errOn fails the n-th call (1-based).
type fakeExec struct {
	scripts []string
	envs    []map[string]string
	errOn   int
}

func (f *fakeExec) run(_ context.Context, _ sshexec.Target, script string, env map[string]string) (*sshexec.Result, error) {
	f.scripts = append(f.scripts, script)
	f.envs = append(f.envs, env)
	if f.errOn == len(f.scripts) {
		return nil, fmt.Errorf("boom")
	}
	return &sshexec.Result{}, nil
}

// fakeBootstrap implements bootstrap.Provider.
type fakeBootstrap struct {
	created     []string
	deleted     []string
	deletedByID []string
}

func (f *fakeBootstrap) CreateToken(context.Context, time.Duration, string) (string, error) {
	token := "abcdef.0123456789abcdef"
	f.created = append(f.created, token)
	return token, nil
}

func (f *fakeBootstrap) DeleteToken(_ context.Context, token string) error {
	f.deleted = append(f.deleted, token)
	return nil
}

func (f *fakeBootstrap) DeleteTokenByID(_ context.Context, tokenID string) error {
	f.deletedByID = append(f.deletedByID, tokenID)
	return nil
}

func (f *fakeBootstrap) ClusterInfo(context.Context, *v1beta1.SSHNodeClass) (*bootstrap.ClusterInfo, error) {
	return &bootstrap.ClusterInfo{Endpoint: "https://cp:6443", CACertB64: "Q0E="}, nil
}

var _ bootstrap.Provider = &fakeBootstrap{}

func testHost(name string) *v1beta1.SSHHost {
	return &v1beta1.SSHHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
			Labels:    map[string]string{v1beta1.HostClassLabel: "big"},
		},
		Spec: v1beta1.SSHHostSpec{
			Address:         "127.0.0.1",
			Port:            1, // instantly refused if anything really dials
			SSHKeySecretRef: corev1.LocalObjectReference{Name: "k"},
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("8"),
				corev1.ResourceMemory: resource.MustParse("62Gi"),
			},
		},
		Status: v1beta1.SSHHostStatus{State: v1beta1.HostStateAvailable},
	}
}

func testNodeClass() *v1beta1.SSHNodeClass {
	return &v1beta1.SSHNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pool"},
		Spec: v1beta1.SSHNodeClassSpec{
			JoinProfileRef: corev1.LocalObjectReference{Name: "prof"},
		},
	}
}

func testProfile() *v1beta1.SSHJoinProfile {
	return &v1beta1.SSHJoinProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "prof"},
		Spec: v1beta1.SSHJoinProfileSpec{
			Version: "1",
			Scripts: v1beta1.ProfileScripts{Install: "echo install", Join: "echo join", Leave: "echo leave"},
		},
	}
}

func testInstanceTypes() []*cloudprovider.InstanceType {
	return []*cloudprovider.InstanceType{{
		Name:      "big",
		Offerings: cloudprovider.Offerings{&cloudprovider.Offering{Price: 0.16}},
	}}
}

func newTestProvider(t *testing.T, exec sshexec.Runner, objs ...runtime.Object) (*DefaultProvider, client.Client, *fakeBootstrap) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(clientgoscheme.Scheme).
		WithStatusSubresource(&v1beta1.SSHHost{}).
		WithRuntimeObjects(objs...).
		// providerID adoption resolves the Node by InternalIP through the
		// field index pkg/operator registers on the real manager; same index
		// function so the two cannot drift.
		WithIndex(&corev1.Node{}, v1beta1.NodeInternalIPIndex,
			func(o client.Object) []string { return v1beta1.NodeInternalIPs(o.(*corev1.Node)) }).
		Build()
	iface := k8sfake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: testNS},
		Data:       map[string][]byte{"privateKey": []byte("dummy")},
	})
	fb := &fakeBootstrap{}
	p := NewDefaultProvider(c, iface, host.NewDefaultProvider(c, testNS), fb)
	if exec != nil {
		p.WithExecutor(exec)
	}
	return p, c, fb
}

func getTestHost(t *testing.T, c client.Client, name string) *v1beta1.SSHHost {
	t.Helper()
	h := &v1beta1.SSHHost{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, h); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestCreateClaimsInstallsJoins(t *testing.T) {
	exec := &fakeExec{}
	p, c, fb := newTestProvider(t, exec.run, testHost("host-a"), testNodeClass(), testProfile())

	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1"}}
	inst, err := p.Create(context.Background(), testNodeClass(), nc, testInstanceTypes())
	if err != nil {
		t.Fatal(err)
	}
	if inst.ProviderID != "kpssh://kpssh-system/host-a" {
		t.Errorf("providerID = %s", inst.ProviderID)
	}
	if len(exec.scripts) != 2 || exec.scripts[0] != "echo install" || exec.scripts[1] != "echo join" {
		t.Fatalf("scripts = %v, want install then join", exec.scripts)
	}
	if got := exec.envs[1]["KPSSH_BOOTSTRAP_TOKEN"]; got != fb.created[0] {
		t.Errorf("join env token = %q", got)
	}
	if got := exec.envs[1]["KPSSH_PROVIDER_ID"]; got != inst.ProviderID {
		t.Errorf("join env providerID = %q (static mode must pass it)", got)
	}
	if !strings.HasPrefix(exec.envs[1]["KPSSH_TAINTS"], karpv1.UnregisteredTaintKey) {
		t.Errorf("unregistered taint must lead: %q", exec.envs[1]["KPSSH_TAINTS"])
	}

	h := getTestHost(t, c, "host-a")
	if h.Status.State != v1beta1.HostStateInUse {
		t.Errorf("state = %s", h.Status.State)
	}
	if h.Status.ClaimRef == nil || h.Status.ClaimRef.Name != "nc-1" {
		t.Errorf("claimRef = %v", h.Status.ClaimRef)
	}
	if h.Status.InstalledProfile != "prof@1" {
		t.Errorf("installedProfile = %q", h.Status.InstalledProfile)
	}
	if h.Status.ProviderID != inst.ProviderID {
		t.Errorf("host providerID = %q", h.Status.ProviderID)
	}
	if h.Status.ClaimRef.UID != nc.UID {
		t.Errorf("claimRef uid = %q, want the NodeClaim's (it fences name reuse)", h.Status.ClaimRef.UID)
	}
	if len(fb.deleted) != 0 || len(fb.deletedByID) != 0 {
		t.Errorf("a token the kubelet may still be using was deleted: %v %v", fb.deleted, fb.deletedByID)
	}
	// The token stays alive until the node registers — but it must be recorded
	// on the host, or nothing would ever collect it (tokencleaner is off).
	if got, want := h.Status.BootstrapTokenID, bootstrap.TokenID(fb.created[0]); got != want {
		t.Errorf("bootstrapTokenID = %q, want %q", got, want)
	}
}

func TestCreateSkipsInstallWhenCached(t *testing.T) {
	h := testHost("host-a")
	h.Status.InstalledProfile = "prof@1"
	exec := &fakeExec{}
	p, _, _ := newTestProvider(t, exec.run, h, testNodeClass(), testProfile())

	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1"}}
	if _, err := p.Create(context.Background(), testNodeClass(), nc, testInstanceTypes()); err != nil {
		t.Fatal(err)
	}
	if len(exec.scripts) != 1 || exec.scripts[0] != "echo join" {
		t.Fatalf("scripts = %v, want join only (install cached)", exec.scripts)
	}
}

func TestCreateJoinFailureReleasesUnhealthyAndDeletesToken(t *testing.T) {
	exec := &fakeExec{errOn: 2} // install ok, join fails
	p, c, fb := newTestProvider(t, exec.run, testHost("host-a"), testNodeClass(), testProfile())

	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1"}}
	_, err := p.Create(context.Background(), testNodeClass(), nc, testInstanceTypes())
	if err == nil {
		t.Fatal("expected error")
	}
	h := getTestHost(t, c, "host-a")
	if h.Status.State != v1beta1.HostStateUnhealthy {
		t.Errorf("state = %s, want Unhealthy", h.Status.State)
	}
	if h.Status.ClaimRef != nil {
		t.Errorf("claimRef must be released: %v", h.Status.ClaimRef)
	}
	if !slices.Equal(fb.deleted, fb.created) {
		t.Errorf("unused token must be deleted: created %v, deleted %v", fb.created, fb.deleted)
	}
	if h.Status.BootstrapTokenID != "" {
		t.Errorf("bootstrapTokenID = %q, want cleared with the token", h.Status.BootstrapTokenID)
	}
}

// A profile misconfiguration (here: a Verified host whose profile carries no
// signature) must not park the host Unhealthy — the host was never touched,
// and one bad profile would otherwise walk every host it is tried against
// into Unhealthy, one per launch attempt.
func TestCreateConfigErrorReleasesHostAvailable(t *testing.T) {
	h := testHost("host-a")
	h.Spec.ExecMode = v1beta1.ExecModeVerified
	p, c, fb := newTestProvider(t, (&fakeExec{}).run, h, testNodeClass(), testProfile())

	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1"}}
	_, err := p.Create(context.Background(), testNodeClass(), nc, testInstanceTypes())
	if err == nil || !strings.Contains(err.Error(), "no signature") {
		t.Fatalf("expected the missing-signature error, got %v", err)
	}
	got := getTestHost(t, c, "host-a")
	if got.Status.State != v1beta1.HostStateAvailable {
		t.Errorf("state = %s, want Available (config errors do not indict the host)", got.Status.State)
	}
	if got.Status.ClaimRef != nil {
		t.Errorf("claimRef must be released: %v", got.Status.ClaimRef)
	}
	if len(fb.created) != 0 {
		t.Errorf("no token should have been minted before the install phase failed: %v", fb.created)
	}
}

func TestCreateNoHostsIsInsufficientCapacity(t *testing.T) {
	p, _, _ := newTestProvider(t, (&fakeExec{}).run, testNodeClass(), testProfile())

	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1"}}
	_, err := p.Create(context.Background(), testNodeClass(), nc, testInstanceTypes())
	if !cloudprovider.IsInsufficientCapacityError(err) {
		t.Fatalf("want InsufficientCapacityError, got %v", err)
	}
}

func TestCreateResumesExistingClaim(t *testing.T) {
	h := testHost("host-a")
	h.Status.State = v1beta1.HostStateClaimed
	h.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc-1"}
	h.Status.InstalledProfile = "prof@1"
	exec := &fakeExec{}
	p, _, _ := newTestProvider(t, exec.run, h, testNodeClass(), testProfile())

	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nc-1"}}
	inst, err := p.Create(context.Background(), testNodeClass(), nc, testInstanceTypes())
	if err != nil {
		t.Fatal(err)
	}
	if inst.HostName != "host-a" {
		t.Fatalf("resume picked %s", inst.HostName)
	}
	if len(exec.scripts) != 1 {
		t.Fatalf("resume must re-join, got scripts %v", exec.scripts)
	}
}

func claimedTestHost() *v1beta1.SSHHost {
	h := testHost("host-a")
	h.Status.State = v1beta1.HostStateInUse
	h.Status.ClaimRef = &v1beta1.ClaimReference{Name: "nc-1", UID: "uid-1"}
	h.Status.InstalledProfile = "prof@1"
	h.Status.ProviderID = "kpssh://kpssh-system/host-a"
	return h
}

// deletingClaim is the NodeClaim karpenter hands to Delete: name, uid and the
// providerID it launched with.
func deletingClaim(name string, uid types.UID) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
	nc.Status.ProviderID = "kpssh://kpssh-system/host-a"
	return nc
}

// TestDeleteLeaveOKReturnsHostWarm: successful leave → host back to Available
// with claim and providerID cleared in one release.
func TestDeleteLeaveOKReturnsHostWarm(t *testing.T) {
	exec := &fakeExec{}
	p, c, _ := newTestProvider(t, exec.run, claimedTestHost(), testProfile())

	if err := p.Delete(context.Background(), deletingClaim("nc-1", "uid-1")); err != nil {
		t.Fatal(err)
	}
	got := getTestHost(t, c, "host-a")
	if got.Status.State != v1beta1.HostStateAvailable {
		t.Errorf("state = %s, want Available", got.Status.State)
	}
	if got.Status.ClaimRef != nil || got.Status.ProviderID != "" {
		t.Errorf("claimRef=%v providerID=%q, want both cleared", got.Status.ClaimRef, got.Status.ProviderID)
	}
	if len(exec.scripts) != 1 || exec.scripts[0] != "echo leave" {
		t.Errorf("scripts = %v, want leave only", exec.scripts)
	}
	if got.Status.InstalledProfile != "prof@1" {
		t.Errorf("install cache must survive leave, got %q", got.Status.InstalledProfile)
	}

	// second Delete: nothing claimed anymore
	err := p.Delete(context.Background(), deletingClaim("nc-1", "uid-1"))
	if !cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("want NodeClaimNotFoundError, got %v", err)
	}
}

// TestDeleteLeaveFailureParksUnhealthy: leave fails → host parked Unhealthy,
// claim and providerID still cleared.
func TestDeleteLeaveFailureParksUnhealthy(t *testing.T) {
	exec := &fakeExec{errOn: 1}
	p, c, _ := newTestProvider(t, exec.run, claimedTestHost(), testProfile())

	if err := p.Delete(context.Background(), deletingClaim("nc-1", "uid-1")); err != nil {
		t.Fatal(err)
	}
	got := getTestHost(t, c, "host-a")
	if got.Status.State != v1beta1.HostStateUnhealthy {
		t.Errorf("state = %s, want Unhealthy", got.Status.State)
	}
	if got.Status.ClaimRef != nil || got.Status.ProviderID != "" {
		t.Errorf("claimRef=%v providerID=%q, want both cleared", got.Status.ClaimRef, got.Status.ProviderID)
	}
}

// TestDeleteFencesForeignClaim is the successor-teardown guard: karpenter
// retries Delete until it reports the claim gone, and a static providerID
// resolves the same host forever — so a late Delete for a dead NodeClaim must
// NOT leave the node of the claim that holds the host now.
func TestDeleteFencesForeignClaim(t *testing.T) {
	for _, tc := range []struct {
		name    string
		deleter *karpv1.NodeClaim
	}{
		{"different name", deletingClaim("nc-0", "uid-0")},
		{"name reused, different uid", deletingClaim("nc-1", "uid-other")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &fakeExec{}
			p, c, _ := newTestProvider(t, exec.run, claimedTestHost(), testProfile())

			err := p.Delete(context.Background(), tc.deleter)
			if !cloudprovider.IsNodeClaimNotFoundError(err) {
				t.Fatalf("want NodeClaimNotFoundError, got %v", err)
			}
			if len(exec.scripts) != 0 {
				t.Errorf("leave must not run against another claim's node: %v", exec.scripts)
			}
			got := getTestHost(t, c, "host-a")
			if got.Status.ClaimRef == nil || got.Status.ClaimRef.Name != "nc-1" {
				t.Errorf("live claim was released: %v", got.Status.ClaimRef)
			}
			if got.Status.State != v1beta1.HostStateInUse {
				t.Errorf("state = %s, want the live claim's InUse", got.Status.State)
			}
		})
	}
}

// TestDeleteCollectsUnspentBootstrapToken: a join that never reached
// registration leaves its token behind; the release path collects it.
func TestDeleteCollectsUnspentBootstrapToken(t *testing.T) {
	h := claimedTestHost()
	h.Status.BootstrapTokenID = "abcdef"
	p, c, fb := newTestProvider(t, (&fakeExec{}).run, h, testProfile())

	if err := p.Delete(context.Background(), deletingClaim("nc-1", "uid-1")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fb.deletedByID, []string{"abcdef"}) {
		t.Errorf("deletedByID = %v, want the host's token", fb.deletedByID)
	}
	if got := getTestHost(t, c, "host-a"); got.Status.BootstrapTokenID != "" {
		t.Errorf("bootstrapTokenID = %q, want cleared", got.Status.BootstrapTokenID)
	}
}

func TestRenderNodeLabels(t *testing.T) {
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				// kubelet-owned namespaces → allowed in --node-labels
				"node.kubernetes.io/instance-type": "big",
				"kubelet.kubernetes.io/whatever":   "x",
				// reserved for central controllers: kubelet refuses to start
				// with it → must be filtered (core syncs it at registration)
				"node-restriction.kubernetes.io/team": "a",
				// non-kubelet kubernetes.io/k8s.io namespaces → filtered
				"topology.k8s.io/zone": "pool",
				// custom namespaces pass (incl. karpenter.sh)
				"karpenter.sh/nodepool": "ssh-pool",
				"custom/label":          "v",
			},
		},
	}
	got := renderNodeLabels(nc)
	for _, want := range []string{
		"node.kubernetes.io/instance-type=big",
		"kubelet.kubernetes.io/whatever=x",
		"karpenter.sh/nodepool=ssh-pool",
		"custom/label=v",
	} {
		if !contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, banned := range []string{"node-restriction.kubernetes.io/team", "topology.k8s.io/zone"} {
		if contains(got, banned+"=a") || contains(got, banned+"=pool") {
			t.Errorf("kubelet-forbidden label %q leaked into %q", banned, got)
		}
	}
}

func contains(csv, part string) bool {
	return slices.Contains(strings.Split(csv, ","), part)
}

func TestRenderTaints(t *testing.T) {
	nc := &karpv1.NodeClaim{
		Spec: karpv1.NodeClaimSpec{
			Taints: []corev1.Taint{
				{Key: "kpssh/pool", Effect: corev1.TaintEffectNoSchedule},
				{Key: "team", Value: "ml", Effect: corev1.TaintEffectNoExecute},
			},
		},
	}
	got := renderTaints(nc)
	// The unregistered taint is karpenter core's registration contract and
	// must ALWAYS lead the list — core removes it once labels/taints synced.
	want := "karpenter.sh/unregistered:NoExecute,kpssh/pool:NoSchedule,team=ml:NoExecute"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	if got := renderTaints(&karpv1.NodeClaim{}); got != "karpenter.sh/unregistered:NoExecute" {
		t.Fatalf("empty claim: got %q", got)
	}
}

func TestSortByPrice(t *testing.T) {
	it := func(name string, prices ...float64) *cloudprovider.InstanceType {
		offs := cloudprovider.Offerings{}
		for _, p := range prices {
			offs = append(offs, &cloudprovider.Offering{Price: p})
		}
		return &cloudprovider.InstanceType{Name: name, Offerings: offs}
	}
	in := []*cloudprovider.InstanceType{it("noOffering"), it("big", 1.28), it("small", 0.04), it("mid", 0.16, 0.08)}
	got := sortByPrice(in)
	if got[0].Name != "small" || got[1].Name != "mid" || got[2].Name != "big" {
		t.Fatalf("order = %s, %s, %s", got[0].Name, got[1].Name, got[2].Name)
	}
	// unpriceable types must sort last, never first
	if got[3].Name != "noOffering" {
		t.Fatalf("type without offerings must sort last, got %s", got[3].Name)
	}
	// input must stay untouched
	if in[0].Name != "noOffering" || in[1].Name != "big" {
		t.Fatal("sortByPrice mutated its input")
	}
}

// adoptProviderID is the EKS Hybrid path: the node registers itself with an
// externally-owned providerID (eks-hybrid:///…/mi-*) and the provider adopts
// it by matching the host's IP to a Node. It resolves the Node through a
// field index — an unregistered index fails at runtime only.
func TestAdoptProviderIDMatchesNodeByInternalIP(t *testing.T) {
	h := &v1beta1.SSHHost{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a", Namespace: testNS},
		Spec:       v1beta1.SSHHostSpec{Address: "10.0.0.11"},
	}
	node := func(name, ip, providerID string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       corev1.NodeSpec{ProviderID: providerID},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: ip},
			}},
		}
	}
	p, _, _ := newTestProvider(t, nil,
		// A decoy on another IP, and one whose IP matches but is ours.
		node("other", "10.0.0.99", "eks-hybrid:///eu-central-1/cluster/mi-decoy"),
		node("ours", "10.0.0.11", "eks-hybrid:///eu-central-1/cluster/mi-0abc"),
	)

	got, err := p.adoptProviderID(context.Background(), h)
	if err != nil {
		t.Fatalf("adoptProviderID: %v", err)
	}
	if want := "eks-hybrid:///eu-central-1/cluster/mi-0abc"; got != want {
		t.Errorf("adopted %q, want %q", got, want)
	}
}

// A Node that matches the IP but carries no providerID yet must not be adopted
// as an empty string — that would hand karpenter a NodeClaim with no instance.
func TestAdoptProviderIDIgnoresNodeWithoutProviderID(t *testing.T) {
	h := &v1beta1.SSHHost{
		ObjectMeta: metav1.ObjectMeta{Name: "host-a", Namespace: testNS},
		Spec:       v1beta1.SSHHostSpec{Address: "10.0.0.11"},
	}
	p, _, _ := newTestProvider(t, nil, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "registering"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.11"},
		}},
	})

	// Bounded so the 4-minute production poll does not stall the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := p.adoptProviderID(ctx, h); err == nil {
		t.Error("adopted a Node that has no providerID yet")
	}
}
