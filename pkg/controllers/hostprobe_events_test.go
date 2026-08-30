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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/events"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

// drain returns all buffered events from the fake recorder.
func drain(rec *events.FakeRecorder) []string {
	out := []string{}
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func observed(cpu string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse("62Gi"),
	}
}

func TestWarnOnCapacityDrift(t *testing.T) {
	t.Run("new shortfall emits one warning", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)
		r := &HostProbeReconciler{Recorder: rec}
		orig := probeHost("host-a", v1beta1.HostStateAvailable) // no observed capacity yet
		host := orig.DeepCopy()
		host.Status.ObservedCapacity = observed("4") // spec says 8

		r.warnOnCapacityDrift(orig, host)
		evs := drain(rec)
		if len(evs) != 1 || !strings.Contains(evs[0], "CapacityDrift") || !strings.Contains(evs[0], "cpu") {
			t.Fatalf("events = %v, want one CapacityDrift mentioning cpu", evs)
		}
	})

	t.Run("persisting shortfall does not re-emit", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)
		r := &HostProbeReconciler{Recorder: rec}
		orig := probeHost("host-a", v1beta1.HostStateAvailable)
		orig.Status.ObservedCapacity = observed("4") // already short before this probe
		host := orig.DeepCopy()
		host.Status.ObservedCapacity = observed("4")

		r.warnOnCapacityDrift(orig, host)
		if evs := drain(rec); len(evs) != 0 {
			t.Fatalf("events = %v, want none while drift persists", evs)
		}
	})

	t.Run("recovered then short again re-emits", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)
		r := &HostProbeReconciler{Recorder: rec}
		orig := probeHost("host-a", v1beta1.HostStateAvailable)
		orig.Status.ObservedCapacity = observed("8") // healthy before this probe
		host := orig.DeepCopy()
		host.Status.ObservedCapacity = observed("2")

		r.warnOnCapacityDrift(orig, host)
		if evs := drain(rec); len(evs) != 1 {
			t.Fatalf("events = %v, want exactly one after recovery→drift", evs)
		}
	})

	t.Run("no shortfall no event", func(t *testing.T) {
		rec := events.NewFakeRecorder(8)
		r := &HostProbeReconciler{Recorder: rec}
		orig := probeHost("host-a", v1beta1.HostStateAvailable)
		host := orig.DeepCopy()
		host.Status.ObservedCapacity = observed("8")

		r.warnOnCapacityDrift(orig, host)
		if evs := drain(rec); len(evs) != 0 {
			t.Fatalf("events = %v, want none", evs)
		}
	})

	t.Run("nil recorder does not panic", func(t *testing.T) {
		r := &HostProbeReconciler{}
		orig := probeHost("host-a", v1beta1.HostStateAvailable)
		host := orig.DeepCopy()
		host.Status.ObservedCapacity = observed("1")
		r.warnOnCapacityDrift(orig, host) // must not panic
	})
}

func TestEventHelper(t *testing.T) {
	rec := events.NewFakeRecorder(4)
	r := &HostProbeReconciler{Recorder: rec}
	h := probeHost("host-a", v1beta1.HostStateAvailable)

	r.event(h, corev1.EventTypeWarning, "ZombieKubelet", "Probe", "kubelet active on unclaimed host")
	evs := drain(rec)
	if len(evs) != 1 || !strings.Contains(evs[0], "ZombieKubelet") {
		t.Fatalf("events = %v, want one ZombieKubelet", evs)
	}

	// nil recorder path must be a no-op, not a panic
	(&HostProbeReconciler{}).event(h, corev1.EventTypeNormal, "X", "Y", "z")
}
