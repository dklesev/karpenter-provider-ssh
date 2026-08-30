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
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

// TestIntegrationCRDSchema exercises the generated CRD schemas against a REAL
// apiserver (envtest): CEL XValidation rules, patterns, enums, integer bounds,
// and defaulting. The fake client evaluates none of these, so only envtest can
// catch a type marker and its generated schema drifting apart.
//
// Requires KUBEBUILDER_ASSETS (run via `make test-integration`).
func TestIntegrationCRDSchema(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set — run via 'make test-integration'")
	}

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	c, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS}}); err != nil {
		t.Fatal(err)
	}

	// mustReject asserts the apiserver refuses the object with a schema
	// validation error (422 Invalid), not some transport/RBAC failure.
	mustReject := func(t *testing.T, obj client.Object, why string) {
		t.Helper()
		err := c.Create(ctx, obj)
		if err == nil {
			t.Fatalf("create %T %q must be rejected (%s), but succeeded", obj, obj.GetName(), why)
		}
		if !apierrors.IsInvalid(err) {
			t.Fatalf("create %T %q: want Invalid (%s), got: %v", obj, obj.GetName(), why, err)
		}
	}
	mustCreate := func(t *testing.T, obj client.Object) {
		t.Helper()
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("create %T %q must succeed, got: %v", obj, obj.GetName(), err)
		}
	}

	// nodeClass returns a minimal valid SSHNodeClass, optionally mutated.
	nodeClass := func(name string, mut func(*v1beta1.SSHNodeClass)) *v1beta1.SSHNodeClass {
		nc := &v1beta1.SSHNodeClass{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: v1beta1.SSHNodeClassSpec{
				JoinProfileRef: corev1.LocalObjectReference{Name: "prof"},
			},
		}
		if mut != nil {
			mut(nc)
		}
		return nc
	}
	// schemaHost returns a minimal valid SSHHost, optionally mutated.
	schemaHost := func(name string, mut func(*v1beta1.SSHHost)) *v1beta1.SSHHost {
		h := &v1beta1.SSHHost{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
			Spec: v1beta1.SSHHostSpec{
				Address:         "10.0.0.1",
				SSHKeySecretRef: corev1.LocalObjectReference{Name: "k"},
				Capacity: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
				},
			},
		}
		if mut != nil {
			mut(h)
		}
		return h
	}

	t.Run("nodeclass vars keys CEL", func(t *testing.T) {
		mustReject(t, nodeClass("nc-vars-bad", func(nc *v1beta1.SSHNodeClass) {
			nc.Spec.Vars = map[string]string{"bad-name": "x"}
		}), "vars key with '-' is not a shell identifier")
		mustCreate(t, nodeClass("nc-vars-good", func(nc *v1beta1.SSHNodeClass) {
			nc.Spec.Vars = map[string]string{"GOOD_name1": "x"}
		}))
	})

	t.Run("nodeclass pricePerCPUHour pattern", func(t *testing.T) {
		mustReject(t, nodeClass("nc-price-bad", func(nc *v1beta1.SSHNodeClass) {
			nc.Spec.PricePerCPUHour = "abc"
		}), "pricePerCPUHour must be a decimal number")
		mustCreate(t, nodeClass("nc-price-good", func(nc *v1beta1.SSHNodeClass) {
			nc.Spec.PricePerCPUHour = "0.02"
		}))
	})

	t.Run("nodeclass providerIDSource enum", func(t *testing.T) {
		mustReject(t, nodeClass("nc-pid-bad", func(nc *v1beta1.SSHNodeClass) {
			nc.Spec.ProviderIDSource = v1beta1.ProviderIDSource("banana")
		}), "providerIDSource is an enum of Static|Adopt")
		// Values are CamelCase per API conventions; a lowercase spelling
		// must not silently pass.
		mustReject(t, nodeClass("nc-pid-lower", func(nc *v1beta1.SSHNodeClass) {
			nc.Spec.ProviderIDSource = v1beta1.ProviderIDSource("adopt")
		}), "lowercase enum values are not accepted")
		mustCreate(t, nodeClass("nc-pid-good", func(nc *v1beta1.SSHNodeClass) {
			nc.Spec.ProviderIDSource = v1beta1.ProviderIDSourceAdopt
		}))
	})

	t.Run("nodeclass empty refs rejected", func(t *testing.T) {
		mustReject(t, nodeClass("nc-ref-empty", func(nc *v1beta1.SSHNodeClass) {
			nc.Spec.JoinProfileRef = corev1.LocalObjectReference{}
		}), "joinProfileRef.name defaults to \"\" without a CEL guard")
		mustReject(t, nodeClass("nc-secret-empty", func(nc *v1beta1.SSHNodeClass) {
			nc.Spec.JoinSecretRef = &corev1.LocalObjectReference{}
		}), "joinSecretRef.name must not be empty")
	})

	t.Run("nodeclass cluster endpoint pattern", func(t *testing.T) {
		mustReject(t, nodeClass("nc-ep-bad", func(nc *v1beta1.SSHNodeClass) {
			nc.Spec.Cluster = &v1beta1.ClusterAccess{Endpoint: "http://insecure"}
		}), "cluster endpoint must be https")
		mustCreate(t, nodeClass("nc-ep-good", func(nc *v1beta1.SSHNodeClass) {
			nc.Spec.Cluster = &v1beta1.ClusterAccess{Endpoint: "https://10.0.0.5:6443"}
		}))
	})

	t.Run("host port bounds", func(t *testing.T) {
		mustReject(t, schemaHost("h-port-high", func(h *v1beta1.SSHHost) {
			h.Spec.Port = 70000
		}), "port above 65535")
		// port 0 would be dropped by omitempty (unset → default 22), so the
		// below-minimum case must use a value that actually serializes.
		mustReject(t, schemaHost("h-port-neg", func(h *v1beta1.SSHHost) {
			h.Spec.Port = -1
		}), "port below 1")
	})

	t.Run("host port and user defaulted", func(t *testing.T) {
		mustCreate(t, schemaHost("h-defaults", nil)) // no port, no user
		got := &v1beta1.SSHHost{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: "h-defaults"}, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Port != 22 {
			t.Errorf("defaulted port = %d, want 22", got.Spec.Port)
		}
		if got.Spec.User != "root" {
			t.Errorf("defaulted user = %q, want root", got.Spec.User)
		}
	})

	t.Run("host address immutable", func(t *testing.T) {
		mustCreate(t, schemaHost("h-immutable", nil))
		got := &v1beta1.SSHHost{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: "h-immutable"}, got); err != nil {
			t.Fatal(err)
		}
		got.Spec.Address = "10.0.0.99"
		if err := c.Update(ctx, got); err == nil || !apierrors.IsInvalid(err) {
			t.Fatalf("address must be immutable (a claimed host would leave against the wrong machine), got: %v", err)
		}
	})

	t.Run("host capacity and nodeAddress guards", func(t *testing.T) {
		mustReject(t, schemaHost("h-cap-nomem", func(h *v1beta1.SSHHost) {
			h.Spec.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}
		}), "capacity without memory models a node nothing fits on")
		// Every one of these passes a naive `^[0-9a-fA-F:.]+$` character
		// class — which is why the CRD validates with CEL isIP instead
		// (see SSHHostSpec.NodeAddress).
		for i, bad := range []string{
			"host.example.com", // a name, which --node-ip rejects outright
			"abc",              // all hex chars, not an address
			"cafe.beef",        // ditto, with a dot to look the part
			"999.999.999.999",  // dotted quad, octets out of range
			":::::",            // colons, no address
			"1.2.3.4.5",        // five octets
		} {
			mustReject(t, schemaHost(fmt.Sprintf("h-nodeaddr-bad-%d", i), func(h *v1beta1.SSHHost) {
				h.Spec.NodeAddress = bad
			}), fmt.Sprintf("nodeAddress %q is not an IP literal; kubelet --node-ip would reject it and the zombie guard would never match a Node", bad))
		}
		for i, good := range []string{"fd00::1", "10.0.0.1", "::1", "2001:db8::8a2e:370:7334"} {
			mustCreate(t, schemaHost(fmt.Sprintf("h-nodeaddr-ok-%d", i), func(h *v1beta1.SSHHost) {
				h.Spec.NodeAddress = good
			}))
		}
	})

	t.Run("host Verified requires trustedSigners", func(t *testing.T) {
		// An empty list would silently drop the controller-side signature
		// check — one of the two gates verified exec promises.
		mustReject(t, schemaHost("h-verified-nosigners", func(h *v1beta1.SSHHost) {
			h.Spec.ExecMode = v1beta1.ExecModeVerified
		}), "execMode Verified with no trustedSigners silently downgrades to one gate")
		mustCreate(t, schemaHost("h-verified-signed", func(h *v1beta1.SSHHost) {
			h.Spec.ExecMode = v1beta1.ExecModeVerified
			h.Spec.TrustedSigners = []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPZa kpssh"}
		}))
		mustCreate(t, schemaHost("h-raw-nosigners", nil)) // Raw needs none
	})

	t.Run("host state enum", func(t *testing.T) {
		mustCreate(t, schemaHost("h-state", nil))
		got := &v1beta1.SSHHost{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: testNS, Name: "h-state"}, got); err != nil {
			t.Fatal(err)
		}
		got.Status.State = v1beta1.HostState("Banana")
		if err := c.Status().Update(ctx, got); err == nil || !apierrors.IsInvalid(err) {
			t.Fatalf("status.state is a closed enum, got: %v", err)
		}
	})

	// The pattern is the guard against a single bad object wedging the profile
	// informer cluster-wide: metav1.Duration cannot decode "banana", and the
	// typed List that fails on it is the one every controller depends on.
	t.Run("joinprofile timeouts pattern", func(t *testing.T) {
		bad := &v1beta1.SSHJoinProfile{ObjectMeta: metav1.ObjectMeta{Name: "prof-bad-timeout"}}
		bad.Spec.Scripts = v1beta1.ProfileScripts{Install: "i", Join: "j", Leave: "l"}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(v1beta1.GroupVersion.WithKind("SSHJoinProfile"))
		u.SetName("prof-bad-timeout")
		if err := unstructured.SetNestedStringMap(u.Object,
			map[string]string{"install": "banana"}, "spec", "timeouts"); err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedStringMap(u.Object,
			map[string]string{"install": "i", "join": "j", "leave": "l"}, "spec", "scripts"); err != nil {
			t.Fatal(err)
		}
		mustReject(t, u, "an undecodable duration would break the typed List of every SSHJoinProfile informer")
	})

	t.Run("joinprofile timeouts round-trip", func(t *testing.T) {
		mustCreate(t, &v1beta1.SSHJoinProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "prof-timeouts"},
			Spec: v1beta1.SSHJoinProfileSpec{
				Scripts: v1beta1.ProfileScripts{Install: "i", Join: "j", Leave: "l"},
				Timeouts: &v1beta1.ProfileTimeouts{
					Install: &metav1.Duration{Duration: 20 * time.Minute},
				},
			},
		})
		got := &v1beta1.SSHJoinProfile{}
		if err := c.Get(ctx, types.NamespacedName{Name: "prof-timeouts"}, got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.Timeouts == nil || got.Spec.Timeouts.Install == nil {
			t.Fatalf("timeouts lost in round-trip: %+v", got.Spec.Timeouts)
		}
		if got.Spec.Timeouts.Install.Duration != 20*time.Minute {
			t.Errorf("install timeout = %s, want 20m", got.Spec.Timeouts.Install.Duration)
		}
	})
}
