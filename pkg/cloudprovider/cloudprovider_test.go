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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/instance"
)

// fakeInstances implements instance.Provider for Get only.
type fakeInstances struct {
	instance.Provider
	inst *instance.Instance
	err  error
}

func (f *fakeInstances) Get(context.Context, string) (*instance.Instance, error) {
	return f.inst, f.err
}

func driftFixture(t *testing.T, installed string) (*CloudProvider, *karpv1.NodeClaim) {
	t.Helper()
	nodeClass := &v1beta1.SSHNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pool"},
		Spec: v1beta1.SSHNodeClassSpec{
			JoinProfileRef: corev1.LocalObjectReference{Name: "prof"},
		},
	}
	prof := &v1beta1.SSHJoinProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "prof"},
		Spec: v1beta1.SSHJoinProfileSpec{
			Version: "2",
			Scripts: v1beta1.ProfileScripts{Install: "i", Join: "j", Leave: "l"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(nodeClass, prof).Build()
	cp := New(c, nil, &fakeInstances{inst: &instance.Instance{InstalledProfile: installed}}, nil)
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "nc-1"},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: v1beta1.GroupVersion.Group, Kind: v1beta1.SSHNodeClassKind, Name: "pool",
			},
		},
	}
	nc.Status.ProviderID = "kpssh://kpssh-system/host-a"
	return cp, nc
}

func TestIsDrifted(t *testing.T) {
	t.Run("marker matches", func(t *testing.T) {
		cp, nc := driftFixture(t, "prof@2")
		reason, err := cp.IsDrifted(context.Background(), nc)
		if err != nil || reason != "" {
			t.Fatalf("reason=%q err=%v", reason, err)
		}
	})

	t.Run("profile version bumped", func(t *testing.T) {
		cp, nc := driftFixture(t, "prof@1")
		reason, err := cp.IsDrifted(context.Background(), nc)
		if err != nil || reason != ProfileDrift {
			t.Fatalf("reason=%q err=%v, want ProfileDrift", reason, err)
		}
	})

	t.Run("no providerID yet", func(t *testing.T) {
		cp, nc := driftFixture(t, "prof@1")
		nc.Status.ProviderID = ""
		reason, err := cp.IsDrifted(context.Background(), nc)
		if err != nil || reason != "" {
			t.Fatalf("reason=%q err=%v", reason, err)
		}
	})

	t.Run("instance gone", func(t *testing.T) {
		cp, nc := driftFixture(t, "")
		cp.instanceProvider = &fakeInstances{err: cloudprovider.NewNodeClaimNotFoundError(context.DeadlineExceeded)}
		reason, err := cp.IsDrifted(context.Background(), nc)
		if err != nil || reason != "" {
			t.Fatalf("reason=%q err=%v", reason, err)
		}
	})

	t.Run("nodeclass hash drift", func(t *testing.T) {
		cp, nc := driftFixture(t, "prof@2")
		// Stamp a hash that cannot match the fixture nodeclass's current spec.
		nc.Annotations = map[string]string{
			v1beta1.AnnotationNodeClassHash:        "0000000000000000",
			v1beta1.AnnotationNodeClassHashVersion: v1beta1.NodeClassHashVersion,
		}
		reason, err := cp.IsDrifted(context.Background(), nc)
		if err != nil || reason != NodeClassDrift {
			t.Fatalf("reason=%q err=%v, want NodeClassDrift", reason, err)
		}
	})

	t.Run("nodeclass hash matches", func(t *testing.T) {
		cp, nc := driftFixture(t, "prof@2")
		nodeClass := &v1beta1.SSHNodeClass{
			Spec: v1beta1.SSHNodeClassSpec{JoinProfileRef: corev1.LocalObjectReference{Name: "prof"}},
		}
		nc.Annotations = map[string]string{
			v1beta1.AnnotationNodeClassHash:        nodeClass.JoinHash(),
			v1beta1.AnnotationNodeClassHashVersion: v1beta1.NodeClassHashVersion,
		}
		reason, err := cp.IsDrifted(context.Background(), nc)
		if err != nil || reason != "" {
			t.Fatalf("reason=%q err=%v", reason, err)
		}
	})

	t.Run("hash version mismatch abstains", func(t *testing.T) {
		cp, nc := driftFixture(t, "prof@2")
		nc.Annotations = map[string]string{
			v1beta1.AnnotationNodeClassHash:        "0000000000000000",
			v1beta1.AnnotationNodeClassHashVersion: "v0",
		}
		reason, err := cp.IsDrifted(context.Background(), nc)
		if err != nil || reason != "" {
			t.Fatalf("reason=%q err=%v, want abstain on version mismatch", reason, err)
		}
	})

	t.Run("unstamped pre-upgrade claim abstains", func(t *testing.T) {
		cp, nc := driftFixture(t, "prof@2")
		reason, err := cp.IsDrifted(context.Background(), nc)
		if err != nil || reason != "" {
			t.Fatalf("reason=%q err=%v, want abstain without annotation", reason, err)
		}
	})
}

func TestJoinHashSensitivity(t *testing.T) {
	base := func() *v1beta1.SSHNodeClass {
		return &v1beta1.SSHNodeClass{Spec: v1beta1.SSHNodeClassSpec{
			Vars: map[string]string{"k8sMinor": "1.34"},
		}}
	}
	h := base().JoinHash()
	if h != base().JoinHash() {
		t.Fatal("JoinHash not deterministic")
	}

	varsChanged := base()
	varsChanged.Spec.Vars["clusterDNS"] = "10.96.0.10"
	if varsChanged.JoinHash() == h {
		t.Error("vars change must change JoinHash")
	}

	clusterChanged := base()
	clusterChanged.Spec.Cluster = &v1beta1.ClusterAccess{Endpoint: "https://cp:6443"}
	if clusterChanged.JoinHash() == h {
		t.Error("cluster change must change JoinHash")
	}

	// Scheduling-model-only fields must NOT roll nodes.
	priceChanged := base()
	priceChanged.Spec.PricePerCPUHour = "9.99"
	if priceChanged.JoinHash() != h {
		t.Error("pricePerCPUHour must not change JoinHash")
	}
	selectorChanged := base()
	selectorChanged.Spec.HostSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "bm"}}
	if selectorChanged.JoinHash() != h {
		t.Error("hostSelector must not change JoinHash")
	}
}

func TestOwnsNodeClassRef(t *testing.T) {
	cases := []struct {
		name string
		ref  *karpv1.NodeClassReference
		want bool
	}{
		{"nil", nil, false},
		{
			"ours",
			&karpv1.NodeClassReference{Group: v1beta1.GroupVersion.Group, Kind: v1beta1.SSHNodeClassKind, Name: "hybrid-pool"},
			true,
		},
		{
			"foreign-aws-ec2nodeclass",
			&karpv1.NodeClassReference{Group: "karpenter.k8s.aws", Kind: "EC2NodeClass", Name: "default"},
			false,
		},
		{
			"right-group-wrong-kind",
			&karpv1.NodeClassReference{Group: v1beta1.GroupVersion.Group, Kind: "SomethingElse", Name: "x"},
			false,
		},
		{
			"right-kind-wrong-group",
			&karpv1.NodeClassReference{Group: "other.example.com", Kind: v1beta1.SSHNodeClassKind, Name: "x"},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownsNodeClassRef(tc.ref); got != tc.want {
				t.Fatalf("ownsNodeClassRef(%+v) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}
