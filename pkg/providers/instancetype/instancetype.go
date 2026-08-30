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

// Package instancetype derives karpenter instance types from the SSHHost pool:
// one instance type per host class, with the class's real capacity and a price
// modeled from the attached-vCPU-hour cost (EKS hybrid economics).
package instancetype

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/host"
)

// PoolZone is the single topology zone the pool reports. Hybrid pools have no
// cloud zones; a stable value keeps karpenter's topology machinery happy.
const PoolZone = "pool"

// defaultPricePerCPUHour is the EKS hybrid attached-vCPU-hour list price,
// used when the node class does not set spec.pricePerCPUHour.
const defaultPricePerCPUHour = 0.02

// defaultMaxPods advertised when the node class does not set spec.maxPods.
const defaultMaxPods = 110

// defaultKubeReserved advertised when the node class does not set
// spec.kubeReserved.
var defaultKubeReserved = corev1.ResourceList{
	corev1.ResourceCPU:    resource.MustParse("80m"),
	corev1.ResourceMemory: resource.MustParse("300Mi"),
}

// Provider lists instance types for a node class.
type Provider interface {
	List(ctx context.Context, nodeClass *v1beta1.SSHNodeClass) ([]*cloudprovider.InstanceType, error)
}

// DefaultProvider implements Provider over the host pool.
type DefaultProvider struct {
	hostProvider host.Provider
}

// NewDefaultProvider returns an instance type provider.
func NewDefaultProvider(hostProvider host.Provider) *DefaultProvider {
	return &DefaultProvider{hostProvider: hostProvider}
}

type classInfo struct {
	name      string
	capacity  corev1.ResourceList
	arch      string
	mixedArch bool
	available int
	total     int
}

// List implements Provider: one instance type per host class visible through
// the node class selector. Available=false offerings keep karpenter informed
// about exhausted classes without hiding their shape. Classes whose arch has
// not been probed yet are omitted — advertising a guessed arch would let
// karpenter schedule incompatible images. An empty result is not an error:
// the node class Ready condition already reports an empty selector match.
func (p *DefaultProvider) List(ctx context.Context, nodeClass *v1beta1.SSHNodeClass) ([]*cloudprovider.InstanceType, error) {
	hosts, err := p.hostProvider.All(ctx, nodeClass.Spec.HostSelector)
	if err != nil {
		return nil, err
	}

	log := ctrllog.FromContext(ctx)
	classes := map[string]*classInfo{}
	for _, h := range hosts {
		if !usableCapacity(h.Spec.Capacity) {
			log.Error(nil, "ignoring host with non-positive cpu/memory capacity",
				"host", h.Name, "capacity", h.Spec.Capacity)
			continue
		}
		class := v1beta1.HostClass(h)
		capacity := withPods(h.Spec.Capacity, nodeClass.Spec.MaxPods)
		ci, ok := classes[class]
		if !ok {
			ci = &classInfo{name: class, capacity: capacity}
			classes[class] = ci
		} else {
			// A class advertises one shape, but Create claims any Available host
			// of it: the shape karpenter bin-packs against must therefore be
			// what the *smallest* member can actually deliver, or pods
			// scheduled onto the in-flight node never fit the host that
			// answers.
			ci.capacity = minCapacity(ci.capacity, capacity)
		}
		switch {
		case h.Status.ObservedArch == "":
		case ci.arch == "":
			ci.arch = h.Status.ObservedArch
		case ci.arch != h.Status.ObservedArch:
			ci.mixedArch = true
		}
		ci.total++
		if h.Status.State == v1beta1.HostStateAvailable && h.Status.ClaimRef == nil {
			ci.available++
		}
	}

	pricePerCPU := defaultPricePerCPUHour
	if v := nodeClass.Spec.PricePerCPUHour; v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid pricePerCPUHour %q on nodeClass %s: %w", v, nodeClass.Name, err)
		}
		pricePerCPU = f
	}

	kubeReserved := defaultKubeReserved
	if len(nodeClass.Spec.KubeReserved) > 0 {
		kubeReserved = nodeClass.Spec.KubeReserved
	}

	out := []*cloudprovider.InstanceType{}
	for _, ci := range classes {
		if ci.arch == "" {
			log.V(1).Info("skipping host class with no probed host yet",
				"nodeClass", nodeClass.Name, "class", ci.name, "hosts", ci.total)
			continue
		}
		if ci.mixedArch {
			// Any host of the class can answer a claim, so a single advertised
			// arch would let karpenter schedule images that cannot run on half
			// the class. Split the hosts into per-arch classes instead.
			log.Error(nil, "skipping host class with hosts of different architectures — label them into separate host classes",
				"nodeClass", nodeClass.Name, "class", ci.name, "hosts", ci.total)
			continue
		}
		cpus := ci.capacity.Cpu().AsApproximateFloat64()
		price := pricePerCPU * cpus

		offerings := cloudprovider.Offerings{
			&cloudprovider.Offering{
				Price:     price,
				Available: ci.available > 0,
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, ci.name),
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, PoolZone),
					scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				),
			},
		}

		out = append(out, &cloudprovider.InstanceType{
			Name: ci.name,
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, ci.name),
				scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, ci.arch),
				scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, string(corev1.Linux)),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, PoolZone),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
			),
			Offerings: offerings,
			Capacity:  ci.capacity,
			Overhead: &cloudprovider.InstanceTypeOverhead{
				KubeReserved: kubeReserved,
			},
		})
	}
	return out, nil
}

// usableCapacity rejects hosts whose declared cpu/memory would model a node
// nothing can be scheduled on (or, after kubeReserved, a negative allocatable).
// The CRD enforces that both keys exist; quantities may still be zero or
// negative.
func usableCapacity(c corev1.ResourceList) bool {
	cpu, memory := c.Cpu(), c.Memory()
	return cpu.Sign() > 0 && memory.Sign() > 0
}

// minCapacity is the per-resource minimum of two capacities. Resources absent
// from either side are dropped: a class may not promise a GPU that only some
// of its hosts carry.
func minCapacity(a, b corev1.ResourceList) corev1.ResourceList {
	out := corev1.ResourceList{}
	for name, qa := range a {
		qb, ok := b[name]
		if !ok {
			continue
		}
		if qb.Cmp(qa) < 0 {
			out[name] = qb.DeepCopy()
		} else {
			out[name] = qa.DeepCopy()
		}
	}
	return out
}

func withPods(c corev1.ResourceList, maxPods *int32) corev1.ResourceList {
	out := c.DeepCopy()
	if maxPods != nil {
		out[corev1.ResourcePods] = *resource.NewQuantity(int64(*maxPods), resource.DecimalSI)
		return out
	}
	if _, ok := out[corev1.ResourcePods]; !ok {
		out[corev1.ResourcePods] = *resource.NewQuantity(defaultMaxPods, resource.DecimalSI)
	}
	return out
}
