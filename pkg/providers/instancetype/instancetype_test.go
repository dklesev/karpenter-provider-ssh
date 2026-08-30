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

package instancetype

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/host"
)

// fakeHosts implements host.Provider for All() only.
type fakeHosts struct {
	host.Provider
	hosts []*v1beta1.SSHHost
}

func (f *fakeHosts) All(_ context.Context, _ *metav1.LabelSelector) ([]*v1beta1.SSHHost, error) {
	return f.hosts, nil
}

func poolHost(name, class, arch string, state v1beta1.HostState, cpu, mem string) *v1beta1.SSHHost {
	return &v1beta1.SSHHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{v1beta1.HostClassLabel: class},
		},
		Spec: v1beta1.SSHHostSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
		},
		Status: v1beta1.SSHHostStatus{State: state, ObservedArch: arch},
	}
}

func TestListDerivesTypesFromClasses(t *testing.T) {
	p := NewDefaultProvider(&fakeHosts{hosts: []*v1beta1.SSHHost{
		poolHost("a", "big", "amd64", v1beta1.HostStateAvailable, "8", "64Gi"),
		poolHost("b", "big", "amd64", v1beta1.HostStateClaimed, "8", "64Gi"),
		poolHost("c", "small", "arm64", v1beta1.HostStateUnhealthy, "2", "4Gi"),
	}})

	nodeClass := &v1beta1.SSHNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pool"},
		Spec:       v1beta1.SSHNodeClassSpec{PricePerCPUHour: "0.02"},
	}
	its, err := p.List(context.Background(), nodeClass)
	if err != nil {
		t.Fatal(err)
	}
	if len(its) != 2 {
		t.Fatalf("want 2 instance types, got %d", len(its))
	}

	byName := map[string]int{}
	for i, it := range its {
		byName[it.Name] = i
	}
	big := its[byName["big"]]
	small := its[byName["small"]]

	// price = vCPUs × pricePerCPUHour
	if got := big.Offerings[0].Price; got != 0.16 {
		t.Errorf("big price = %v, want 0.16", got)
	}
	// class with one Available host → offering available
	if !big.Offerings[0].Available {
		t.Error("big should be available (host a)")
	}
	// class with no Available host → offering kept but unavailable
	if small.Offerings[0].Available {
		t.Error("small must be unavailable (only unhealthy hosts)")
	}
	// pods default injected
	if big.Capacity.Pods().Value() != 110 {
		t.Errorf("pods = %v, want 110", big.Capacity.Pods().Value())
	}
	// arch requirement from observation
	if !small.Requirements.Get(corev1.LabelArchStable).Has(string(karpv1.ArchitectureArm64)) {
		t.Error("small must require arm64")
	}
}

func TestListEmptyPoolReturnsNoTypes(t *testing.T) {
	// An empty pool is not an error — the nodeclass Ready condition reports it;
	// erroring here would make karpenter core log failures for a state that is
	// simply "no capacity".
	p := NewDefaultProvider(&fakeHosts{})
	its, err := p.List(context.Background(), &v1beta1.SSHNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "pool"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(its) != 0 {
		t.Fatalf("want no instance types, got %d", len(its))
	}
}

func TestListSkipsUnprobedClasses(t *testing.T) {
	p := NewDefaultProvider(&fakeHosts{hosts: []*v1beta1.SSHHost{
		poolHost("a", "big", "", v1beta1.HostStatePending, "8", "64Gi"), // not probed yet
		poolHost("b", "small", "amd64", v1beta1.HostStateAvailable, "2", "4Gi"),
	}})
	its, err := p.List(context.Background(), &v1beta1.SSHNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "pool"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(its) != 1 || its[0].Name != "small" {
		t.Fatalf("want only the probed class, got %+v", its)
	}
}

func TestListInvalidPriceErrors(t *testing.T) {
	p := NewDefaultProvider(&fakeHosts{hosts: []*v1beta1.SSHHost{
		poolHost("a", "big", "amd64", v1beta1.HostStateAvailable, "8", "64Gi"),
	}})
	nodeClass := &v1beta1.SSHNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pool"},
		Spec:       v1beta1.SSHNodeClassSpec{PricePerCPUHour: "not-a-number"},
	}
	if _, err := p.List(context.Background(), nodeClass); err == nil {
		t.Fatal("expected error for invalid pricePerCPUHour")
	}
}

func TestListNodeClassOverrides(t *testing.T) {
	p := NewDefaultProvider(&fakeHosts{hosts: []*v1beta1.SSHHost{
		poolHost("a", "big", "amd64", v1beta1.HostStateAvailable, "8", "64Gi"),
	}})
	maxPods := int32(42)
	nodeClass := &v1beta1.SSHNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pool"},
		Spec: v1beta1.SSHNodeClassSpec{
			MaxPods: &maxPods,
			KubeReserved: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("200m"),
			},
		},
	}
	its, err := p.List(context.Background(), nodeClass)
	if err != nil {
		t.Fatal(err)
	}
	if got := its[0].Capacity.Pods().Value(); got != 42 {
		t.Errorf("pods = %d, want 42", got)
	}
	if got := its[0].Overhead.KubeReserved.Cpu().String(); got != "200m" {
		t.Errorf("kubeReserved cpu = %s, want 200m", got)
	}
}

// TestListNormalizesUnlabeledHostsToDefaultClass is the phantom-class guard:
// if an unlabeled host were advertised as instance type "default" while the
// claim path matched on the (empty) label, every launch against it would fail
// with insufficient capacity, karpenter would delete the NodeClaim and the
// provisioner would try again, forever. Advertising and claiming must agree.
func TestListNormalizesUnlabeledHostsToDefaultClass(t *testing.T) {
	unlabeled := poolHost("u", "", "amd64", v1beta1.HostStateAvailable, "4", "16Gi")
	unlabeled.Labels = nil

	its, err := NewDefaultProvider(&fakeHosts{hosts: []*v1beta1.SSHHost{unlabeled}}).
		List(context.Background(), &v1beta1.SSHNodeClass{})
	if err != nil {
		t.Fatal(err)
	}
	if len(its) != 1 || its[0].Name != v1beta1.DefaultHostClass {
		t.Fatalf("instance types = %v, want one named %q", its, v1beta1.DefaultHostClass)
	}
	if got := v1beta1.HostClass(unlabeled); got != its[0].Name {
		t.Fatalf("claim path resolves class %q but %q is advertised — nothing could ever be claimed", got, its[0].Name)
	}
}

// A class advertises one shape but Create claims any Available host of it, so
// the shape karpenter bin-packs against must be what the smallest member can
// deliver — and a resource only some hosts have must not be promised at all.
func TestListAdvertisesClassMinimumCapacity(t *testing.T) {
	big := poolHost("big", "mixed", "amd64", v1beta1.HostStateAvailable, "16", "64Gi")
	big.Spec.Capacity["nvidia.com/gpu"] = resource.MustParse("1")
	small := poolHost("small", "mixed", "amd64", v1beta1.HostStateAvailable, "4", "16Gi")

	its, err := NewDefaultProvider(&fakeHosts{hosts: []*v1beta1.SSHHost{big, small}}).
		List(context.Background(), &v1beta1.SSHNodeClass{})
	if err != nil {
		t.Fatal(err)
	}
	if len(its) != 1 {
		t.Fatalf("instance types = %d, want 1", len(its))
	}
	if cpu := its[0].Capacity.Cpu(); cpu.Value() != 4 {
		t.Errorf("advertised cpu = %s, want the smallest host's 4", cpu)
	}
	want16Gi := resource.MustParse("16Gi")
	if mem := its[0].Capacity.Memory(); mem.Value() != want16Gi.Value() {
		t.Errorf("advertised memory = %s, want the smallest host's 16Gi", mem)
	}
	if _, ok := its[0].Capacity["nvidia.com/gpu"]; ok {
		t.Error("a GPU only one host carries must not be advertised for the class")
	}
}

// Any host of a class can answer a claim, so a class straddling architectures
// cannot be advertised with one of them: pods would be scheduled with images
// that cannot run on half its hosts.
func TestListSkipsMixedArchClass(t *testing.T) {
	its, err := NewDefaultProvider(&fakeHosts{hosts: []*v1beta1.SSHHost{
		poolHost("a", "mixed", "amd64", v1beta1.HostStateAvailable, "8", "32Gi"),
		poolHost("b", "mixed", "arm64", v1beta1.HostStateAvailable, "8", "32Gi"),
		poolHost("c", "clean", "arm64", v1beta1.HostStateAvailable, "8", "32Gi"),
	}}).List(context.Background(), &v1beta1.SSHNodeClass{})
	if err != nil {
		t.Fatal(err)
	}
	if len(its) != 1 || its[0].Name != "clean" {
		t.Fatalf("instance types = %v, want only the single-arch class", its)
	}
}

// A zero/negative capacity would model a node nothing schedules on (and, after
// kubeReserved, a negative allocatable). The CRD keeps the keys present; the
// quantities are still the operator's to get wrong.
func TestListIgnoresHostsWithUnusableCapacity(t *testing.T) {
	its, err := NewDefaultProvider(&fakeHosts{hosts: []*v1beta1.SSHHost{
		poolHost("zero", "z", "amd64", v1beta1.HostStateAvailable, "0", "16Gi"),
	}}).List(context.Background(), &v1beta1.SSHNodeClass{})
	if err != nil {
		t.Fatal(err)
	}
	if len(its) != 0 {
		t.Fatalf("instance types = %v, want none (the only host has 0 cpu)", its)
	}
}
