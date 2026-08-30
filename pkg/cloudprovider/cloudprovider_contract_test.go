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

package cloudprovider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
	karpscheduling "sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/instance"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/instancetype"
)

// fakeInstanceLifecycle implements instance.Provider with canned results.
type fakeInstanceLifecycle struct {
	created   *instance.Instance
	createErr error
	deleted   []string
	instances []*instance.Instance
}

func (f *fakeInstanceLifecycle) Create(_ context.Context, _ *v1beta1.SSHNodeClass, _ *karpv1.NodeClaim, _ []*cloudprovider.InstanceType) (*instance.Instance, error) {
	return f.created, f.createErr
}

func (f *fakeInstanceLifecycle) Delete(_ context.Context, nodeClaim *karpv1.NodeClaim) error {
	f.deleted = append(f.deleted, nodeClaim.Status.ProviderID)
	return nil
}

func (f *fakeInstanceLifecycle) Get(_ context.Context, providerID string) (*instance.Instance, error) {
	for _, in := range f.instances {
		if in.ProviderID == providerID {
			return in, nil
		}
	}
	return nil, cloudprovider.NewNodeClaimNotFoundError(errors.New("not found"))
}

func (f *fakeInstanceLifecycle) List(context.Context) ([]*instance.Instance, error) {
	return f.instances, nil
}

// fakeInstanceTypes implements instancetype.Provider with a canned list.
type fakeInstanceTypes struct {
	types  []*cloudprovider.InstanceType
	called int
}

func (f *fakeInstanceTypes) List(context.Context, *v1beta1.SSHNodeClass) ([]*cloudprovider.InstanceType, error) {
	f.called++
	return f.types, nil
}

func poolInstanceType(name, arch string) *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{
		Name: name,
		Requirements: karpscheduling.NewRequirements(
			karpscheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, name),
			karpscheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, arch),
			karpscheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, string(corev1.Linux)),
			karpscheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, instancetype.PoolZone),
			karpscheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		),
		Offerings: cloudprovider.Offerings{&cloudprovider.Offering{
			Price:     1,
			Available: true,
			Requirements: karpscheduling.NewRequirements(
				karpscheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, name),
				karpscheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, instancetype.PoolZone),
				karpscheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
			),
		}},
		Capacity: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		},
		Overhead: &cloudprovider.InstanceTypeOverhead{
			KubeReserved: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}
}

type contractFixture struct {
	cp        *CloudProvider
	instances *fakeInstanceLifecycle
	types     *fakeInstanceTypes
	recorder  *record.FakeRecorder
}

func newContractFixture(t *testing.T, readyNodeClass bool) *contractFixture {
	t.Helper()
	nodeClass := &v1beta1.SSHNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pool"},
		Spec: v1beta1.SSHNodeClassSpec{
			JoinProfileRef: corev1.LocalObjectReference{Name: "prof"},
		},
	}
	if readyNodeClass {
		nodeClass.StatusConditions().SetTrue(status.ConditionReady)
	} else {
		nodeClass.StatusConditions().SetFalse(status.ConditionReady, "HostsNotReady", "no probed hosts")
	}
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithStatusSubresource(&v1beta1.SSHNodeClass{}).WithObjects(nodeClass).Build()
	fi := &fakeInstanceLifecycle{}
	ft := &fakeInstanceTypes{types: []*cloudprovider.InstanceType{poolInstanceType("bm-large", "amd64")}}
	rec := record.NewFakeRecorder(16)
	return &contractFixture{
		cp:        New(c, events.NewRecorder(rec), fi, ft),
		instances: fi,
		types:     ft,
		recorder:  rec,
	}
}

func contractNodeClaim() *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "nc-1"},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: v1beta1.GroupVersion.Group, Kind: v1beta1.SSHNodeClassKind, Name: "pool",
			},
		},
	}
}

func TestCreate(t *testing.T) {
	t.Run("node class not ready", func(t *testing.T) {
		f := newContractFixture(t, false)
		_, err := f.cp.Create(context.Background(), contractNodeClaim())
		if !cloudprovider.IsNodeClassNotReadyError(err) {
			t.Fatalf("err = %v, want NodeClassNotReadyError", err)
		}
	})

	t.Run("missing node class is insufficient capacity", func(t *testing.T) {
		f := newContractFixture(t, true)
		nc := contractNodeClaim()
		nc.Spec.NodeClassRef.Name = "does-not-exist"
		_, err := f.cp.Create(context.Background(), nc)
		if !cloudprovider.IsInsufficientCapacityError(err) {
			t.Fatalf("err = %v, want InsufficientCapacityError", err)
		}
	})

	t.Run("no compatible instance types is insufficient capacity", func(t *testing.T) {
		f := newContractFixture(t, true)
		f.types.types = nil
		_, err := f.cp.Create(context.Background(), contractNodeClaim())
		if !cloudprovider.IsInsufficientCapacityError(err) {
			t.Fatalf("err = %v, want InsufficientCapacityError", err)
		}
	})

	t.Run("instance create failure publishes event and propagates", func(t *testing.T) {
		f := newContractFixture(t, true)
		f.instances.createErr = errors.New("join script exploded")
		_, err := f.cp.Create(context.Background(), contractNodeClaim())
		if err == nil || !errors.Is(err, f.instances.createErr) {
			t.Fatalf("err = %v, want create error", err)
		}
		select {
		case ev := <-f.recorder.Events:
			if ev == "" {
				t.Fatal("empty event")
			}
		default:
			t.Fatal("no event published on create failure")
		}
	})

	t.Run("success maps instance to node claim", func(t *testing.T) {
		f := newContractFixture(t, true)
		created := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
		f.instances.created = &instance.Instance{
			ProviderID:   "kpssh://kpssh-system/host-a",
			NodeName:     "host-a",
			Class:        "bm-large",
			CreationTime: created,
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"),
			},
		}
		got, err := f.cp.Create(context.Background(), contractNodeClaim())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got.Name != "host-a" || got.Status.ProviderID != "kpssh://kpssh-system/host-a" {
			t.Fatalf("mapped claim = %+v", got)
		}
		wantLabels := map[string]string{
			corev1.LabelInstanceTypeStable: "bm-large",
			corev1.LabelTopologyZone:       instancetype.PoolZone,
			karpv1.CapacityTypeLabelKey:    karpv1.CapacityTypeOnDemand,
		}
		for k, v := range wantLabels {
			if got.Labels[k] != v {
				t.Errorf("label %s = %q, want %q", k, got.Labels[k], v)
			}
		}
		if !got.CreationTimestamp.Time.Equal(created) {
			t.Errorf("creationTimestamp = %v, want %v", got.CreationTimestamp, created)
		}
		if got.Status.Capacity.Cpu().String() != "4" {
			t.Errorf("capacity cpu = %s", got.Status.Capacity.Cpu())
		}
		// In-flight capacity modeling: core copies these onto the launched
		// NodeClaim; without pods + allocatable it over-provisions.
		if got.Status.Capacity.Pods().String() != "110" {
			t.Errorf("capacity pods = %s, want 110", got.Status.Capacity.Pods())
		}
		if got.Status.Allocatable.Cpu().String() != "3900m" {
			t.Errorf("allocatable cpu = %s, want 3900m (4 - 100m kubeReserved)", got.Status.Allocatable.Cpu())
		}
		if got.Annotations[v1beta1.AnnotationNodeClassHash] == "" ||
			got.Annotations[v1beta1.AnnotationNodeClassHashVersion] != v1beta1.NodeClassHashVersion {
			t.Errorf("nodeclass hash annotations not stamped: %v", got.Annotations)
		}
	})
}

func TestResolveInstanceTypesFiltering(t *testing.T) {
	newClaim := func(reqs ...karpv1.NodeSelectorRequirementWithMinValues) *karpv1.NodeClaim {
		nc := contractNodeClaim()
		nc.Spec.Requirements = reqs
		return nc
	}
	req := func(key string, op corev1.NodeSelectorOperator, vals ...string) karpv1.NodeSelectorRequirementWithMinValues {
		return karpv1.NodeSelectorRequirementWithMinValues{Key: key, Operator: op, Values: vals}
	}

	cases := []struct {
		name  string
		types []*cloudprovider.InstanceType
		reqs  []karpv1.NodeSelectorRequirementWithMinValues
		want  []string
	}{
		{
			name:  "no requirements keeps all",
			types: []*cloudprovider.InstanceType{poolInstanceType("bm-large", "amd64"), poolInstanceType("bm-small", "arm64")},
			want:  []string{"bm-large", "bm-small"},
		},
		{
			name:  "instance-type restriction filters",
			types: []*cloudprovider.InstanceType{poolInstanceType("bm-large", "amd64"), poolInstanceType("bm-small", "arm64")},
			reqs:  []karpv1.NodeSelectorRequirementWithMinValues{req(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "bm-small")},
			want:  []string{"bm-small"},
		},
		{
			name:  "arch requirement filters",
			types: []*cloudprovider.InstanceType{poolInstanceType("bm-large", "amd64"), poolInstanceType("bm-small", "arm64")},
			reqs:  []karpv1.NodeSelectorRequirementWithMinValues{req(corev1.LabelArchStable, corev1.NodeSelectorOpIn, "arm64")},
			want:  []string{"bm-small"},
		},
		{
			name:  "spot requirement excludes on-demand pool",
			types: []*cloudprovider.InstanceType{poolInstanceType("bm-large", "amd64")},
			reqs:  []karpv1.NodeSelectorRequirementWithMinValues{req(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeSpot)},
			want:  []string{},
		},
		{
			name:  "undefined well-known label tolerated",
			types: []*cloudprovider.InstanceType{poolInstanceType("bm-large", "amd64")},
			reqs:  []karpv1.NodeSelectorRequirementWithMinValues{req(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "on-prem-1")},
			want:  []string{"bm-large"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newContractFixture(t, true)
			f.types.types = tc.types
			nodeClass := &v1beta1.SSHNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "pool"}}
			got, err := f.cp.resolveInstanceTypes(context.Background(), newClaim(tc.reqs...), nodeClass)
			if err != nil {
				t.Fatalf("resolveInstanceTypes: %v", err)
			}
			names := make([]string, 0, len(got))
			for _, it := range got {
				names = append(names, it.Name)
			}
			if len(names) != len(tc.want) {
				t.Fatalf("got %v, want %v", names, tc.want)
			}
			for i := range names {
				if names[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", names, tc.want)
				}
			}
		})
	}
}

func TestDelete(t *testing.T) {
	// A NodeClaim that never launched has nothing to tear down. nil would mean
	// "termination in progress" to core, which then requeues forever.
	t.Run("empty providerID is NodeClaimNotFound", func(t *testing.T) {
		f := newContractFixture(t, true)
		err := f.cp.Delete(context.Background(), contractNodeClaim())
		if !cloudprovider.IsNodeClaimNotFoundError(err) {
			t.Fatalf("want NodeClaimNotFoundError, got %v", err)
		}
		if len(f.instances.deleted) != 0 {
			t.Fatalf("instance delete called for empty providerID: %v", f.instances.deleted)
		}
	})

	t.Run("delegates to instance provider", func(t *testing.T) {
		f := newContractFixture(t, true)
		nc := contractNodeClaim()
		nc.Status.ProviderID = "kpssh://kpssh-system/host-a"
		if err := f.cp.Delete(context.Background(), nc); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if len(f.instances.deleted) != 1 || f.instances.deleted[0] != nc.Status.ProviderID {
			t.Fatalf("deleted = %v", f.instances.deleted)
		}
	})
}

func TestGetAndList(t *testing.T) {
	f := newContractFixture(t, true)
	f.instances.instances = []*instance.Instance{
		{ProviderID: "kpssh://ns/host-a", NodeName: "host-a", Class: "bm-large"},
		{ProviderID: "kpssh://ns/host-b", NodeName: "host-b", Class: "bm-small"},
	}

	t.Run("get empty providerID errors", func(t *testing.T) {
		if _, err := f.cp.Get(context.Background(), ""); err == nil {
			t.Fatal("want error for empty providerID")
		}
	})

	t.Run("get maps instance", func(t *testing.T) {
		got, err := f.cp.Get(context.Background(), "kpssh://ns/host-b")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Name != "host-b" || got.Labels[corev1.LabelInstanceTypeStable] != "bm-small" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("list maps all instances", func(t *testing.T) {
		got, err := f.cp.List(context.Background())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 || got[0].Status.ProviderID != "kpssh://ns/host-a" || got[1].Status.ProviderID != "kpssh://ns/host-b" {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestGetInstanceTypes(t *testing.T) {
	t.Run("nil pool and foreign pools are a no-op", func(t *testing.T) {
		f := newContractFixture(t, true)
		got, err := f.cp.GetInstanceTypes(context.Background(), nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("nil pool: got %v, %v", got, err)
		}
		foreign := &karpv1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: "aws"},
			Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
				NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.k8s.aws", Kind: "EC2NodeClass", Name: "default"},
			}}},
		}
		got, err = f.cp.GetInstanceTypes(context.Background(), foreign)
		if err != nil || len(got) != 0 {
			t.Fatalf("foreign pool: got %v, %v", got, err)
		}
		if f.types.called != 0 {
			t.Fatalf("instance type provider consulted for foreign pool")
		}
	})

	t.Run("own pool lists types", func(t *testing.T) {
		f := newContractFixture(t, true)
		pool := &karpv1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: "hybrid"},
			Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
				NodeClassRef: &karpv1.NodeClassReference{Group: v1beta1.GroupVersion.Group, Kind: v1beta1.SSHNodeClassKind, Name: "pool"},
			}}},
		}
		got, err := f.cp.GetInstanceTypes(context.Background(), pool)
		if err != nil || len(got) != 1 || got[0].Name != "bm-large" {
			t.Fatalf("got %v, %v", got, err)
		}
	})

	t.Run("own pool with missing node class errors", func(t *testing.T) {
		f := newContractFixture(t, true)
		pool := &karpv1.NodePool{
			ObjectMeta: metav1.ObjectMeta{Name: "hybrid"},
			Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{
				NodeClassRef: &karpv1.NodeClassReference{Group: v1beta1.GroupVersion.Group, Kind: v1beta1.SSHNodeClassKind, Name: "gone"},
			}}},
		}
		if _, err := f.cp.GetInstanceTypes(context.Background(), pool); err == nil {
			t.Fatal("want error for missing node class")
		}
	})
}

// Until the node registers, the NodeClaim's labels are all karpenter knows
// about the shape of what is coming. Without arch/os, a pod selecting on
// kubernetes.io/arch cannot be modelled onto the in-flight node — and core
// launches a second one.
func TestCreateStampsInstanceTypeLabels(t *testing.T) {
	f := newContractFixture(t, true)
	f.instances.created = &instance.Instance{
		ProviderID: "kpssh://kpssh-system/host-a",
		NodeName:   "nc-1",
		Class:      "bm-large",
	}

	created, err := f.cp.Create(context.Background(), contractNodeClaim())
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		corev1.LabelInstanceTypeStable: "bm-large",
		corev1.LabelArchStable:         "amd64",
		corev1.LabelOSStable:           string(corev1.Linux),
		corev1.LabelTopologyZone:       instancetype.PoolZone,
		karpv1.CapacityTypeLabelKey:    karpv1.CapacityTypeOnDemand,
	} {
		if got := created.Labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}
}

// InsufficientCapacity is a verdict: core deletes the NodeClaim and
// re-simulates instead of retrying the launch. Only a genuinely absent node
// class may say that — an apiserver hiccup may not.
func TestCreateTransientNodeClassErrorIsRetryable(t *testing.T) {
	nodeClass := &v1beta1.SSHNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "pool"}}
	nodeClass.StatusConditions().SetTrue(status.ConditionReady)
	boom := errors.New("etcdserver: request timed out")
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithStatusSubresource(&v1beta1.SSHNodeClass{}).WithObjects(nodeClass).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return boom
			},
		}).Build()
	cp := New(c, events.NewRecorder(record.NewFakeRecorder(16)), &fakeInstanceLifecycle{},
		&fakeInstanceTypes{types: []*cloudprovider.InstanceType{poolInstanceType("bm-large", "amd64")}})

	_, err := cp.Create(context.Background(), contractNodeClaim())
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the transient error", err)
	}
	if cloudprovider.IsInsufficientCapacityError(err) {
		t.Fatal("a transient API error must not be reported as insufficient capacity — core would delete the NodeClaim")
	}
}
