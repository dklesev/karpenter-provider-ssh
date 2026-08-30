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

// Package cloudprovider implements the karpenter CloudProvider over an
// SSH-reachable host pool: membership, not machines, is what scales.
package cloudprovider

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
	karpscheduling "sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/dklesev/karpenter-provider-ssh/internal/profile"
	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/instance"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/instancetype"
)

// Name of this cloud provider.
const Name = "ssh"

// CloudProvider implements karpenter's cloudprovider.CloudProvider.
type CloudProvider struct {
	kubeClient client.Client
	recorder   events.Recorder

	instanceProvider     instance.Provider
	instanceTypeProvider instancetype.Provider
}

// New returns the SSH pool cloud provider.
func New(kubeClient client.Client, recorder events.Recorder, instanceProvider instance.Provider, instanceTypeProvider instancetype.Provider) *CloudProvider {
	return &CloudProvider{
		kubeClient:           kubeClient,
		recorder:             recorder,
		instanceProvider:     instanceProvider,
		instanceTypeProvider: instanceTypeProvider,
	}
}

// Create claims + joins a pool host for the NodeClaim.
func (c *CloudProvider) Create(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	nodeClass, err := c.nodeClassByRef(ctx, nodeClaim.Spec.NodeClassRef)
	if err != nil {
		// Only a genuinely missing node class is a capacity verdict: core
		// deletes the NodeClaim on InsufficientCapacity, so labelling a
		// transient API error that way would throw away a launch that merely
		// needed a retry.
		if apierrors.IsNotFound(err) {
			return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("resolving node class, %w", err))
		}
		return nil, fmt.Errorf("resolving node class, %w", err)
	}
	ready := nodeClass.StatusConditions().Get(status.ConditionReady)
	if ready.IsFalse() {
		return nil, cloudprovider.NewNodeClassNotReadyError(fmt.Errorf("%s", ready.Message))
	}
	if !ready.IsTrue() {
		// Freshly created nodeclass whose readiness controller hasn't run yet.
		return nil, cloudprovider.NewCreateError(
			fmt.Errorf("resolving node class readiness, NodeClass is in Ready=Unknown"),
			"NodeClassReadinessUnknown", "NodeClass is in Ready=Unknown")
	}

	instanceTypes, err := c.resolveInstanceTypes(ctx, nodeClaim, nodeClass)
	if err != nil {
		return nil, err
	}
	if len(instanceTypes) == 0 {
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("no compatible instance types for NodeClaim %s", nodeClaim.Name))
	}

	inst, err := c.instanceProvider.Create(ctx, nodeClass, nodeClaim, instanceTypes)
	if err != nil {
		c.recorder.Publish(createFailedEvent(nodeClaim, err))
		return nil, err
	}
	created := c.instanceToNodeClaim(inst, instanceTypes...)
	// Core copies these onto the launched NodeClaim; IsDrifted compares them
	// against the nodeclass's current spec.
	created.Annotations = map[string]string{
		v1beta1.AnnotationNodeClassHash:        nodeClass.JoinHash(),
		v1beta1.AnnotationNodeClassHashVersion: v1beta1.NodeClassHashVersion,
	}
	return created, nil
}

// Delete leaves + releases the host behind the NodeClaim.
func (c *CloudProvider) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	if nodeClaim.Status.ProviderID == "" {
		// Nothing was ever launched for this claim. nil would mean "termination
		// in progress" and core would retry forever; NotFound ends it.
		return cloudprovider.NewNodeClaimNotFoundError(
			fmt.Errorf("nodeClaim %s has no providerID", nodeClaim.Name))
	}
	return c.instanceProvider.Delete(ctx, nodeClaim)
}

// Get returns the NodeClaim view of a claimed host.
func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	if providerID == "" {
		return nil, fmt.Errorf("empty providerID")
	}
	inst, err := c.instanceProvider.Get(ctx, providerID)
	if err != nil {
		return nil, err
	}
	return c.instanceToNodeClaim(inst), nil
}

// List returns NodeClaim views for all claimed hosts.
func (c *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	instances, err := c.instanceProvider.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*karpv1.NodeClaim, 0, len(instances))
	for _, inst := range instances {
		out = append(out, c.instanceToNodeClaim(inst))
	}
	return out, nil
}

// GetInstanceTypes derives instance types (host classes) for a NodePool.
// karpenter core invokes this for EVERY NodePool in the cluster (e.g. the
// state.pricing controller), so we must no-op on pools owned by a coexisting
// cloudprovider instead of erroring while trying to resolve a foreign
// nodeClassRef as one of our SSHNodeClasses.
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*cloudprovider.InstanceType, error) {
	if nodePool == nil || !ownsNodeClassRef(nodePool.Spec.Template.Spec.NodeClassRef) {
		return []*cloudprovider.InstanceType{}, nil
	}
	nodeClass, err := c.nodeClassByRef(ctx, nodePool.Spec.Template.Spec.NodeClassRef)
	if err != nil {
		return nil, fmt.Errorf("resolving node class, %w", err)
	}
	return c.instanceTypeProvider.List(ctx, nodeClass)
}

// ownsNodeClassRef reports whether a nodeClassRef targets our SSHNodeClass
// (matched by API group + kind) rather than another cloudprovider's node class.
func ownsNodeClassRef(ref *karpv1.NodeClassReference) bool {
	return ref != nil &&
		ref.Group == v1beta1.GroupVersion.Group &&
		ref.Kind == v1beta1.SSHNodeClassKind
}

// ProfileDrift reports that the host's installed profile marker no longer
// matches the node class's profile — bumping SSHJoinProfile.spec.version
// rolls affected nodes through karpenter's drift disruption.
const ProfileDrift cloudprovider.DriftReason = "ProfileDrift"

// NodeClassDrift reports that join-affecting SSHNodeClass spec fields changed
// since the NodeClaim was created (compared via the nodeclass-hash annotation
// stamped at Create).
const NodeClassDrift cloudprovider.DriftReason = "NodeClassDrift"

// IsDrifted compares the backing host's install-cache marker
// ("<profile>@<version>") with the node class's current join profile.
func (c *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim) (cloudprovider.DriftReason, error) {
	if nodeClaim.Status.ProviderID == "" {
		return "", nil
	}
	inst, err := c.instanceProvider.Get(ctx, nodeClaim.Status.ProviderID)
	if err != nil {
		if cloudprovider.IsNodeClaimNotFoundError(err) {
			return "", nil
		}
		return "", err
	}
	nodeClass, err := c.nodeClassByRef(ctx, nodeClaim.Spec.NodeClassRef)
	if err != nil {
		return "", fmt.Errorf("resolving node class, %w", err)
	}
	prof := &v1beta1.SSHJoinProfile{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodeClass.Spec.JoinProfileRef.Name}, prof); err != nil {
		return "", fmt.Errorf("resolving join profile, %w", err)
	}
	if inst.InstalledProfile != "" && inst.InstalledProfile != profile.InstalledMarker(prof) {
		return ProfileDrift, nil
	}
	// Static drift on join-affecting nodeclass fields. Claims without the
	// annotation (pre-upgrade) or with a different hash version abstain —
	// never roll the fleet because the algorithm changed.
	stamped := nodeClaim.Annotations[v1beta1.AnnotationNodeClassHash]
	version := nodeClaim.Annotations[v1beta1.AnnotationNodeClassHashVersion]
	if stamped != "" && version == v1beta1.NodeClassHashVersion && stamped != nodeClass.JoinHash() {
		return NodeClassDrift, nil
	}
	return "", nil
}

// RepairPolicies lets karpenter force-terminate NodeClaims on persistently
// unhealthy nodes; the host then goes back through the probe loop.
func (c *CloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return []cloudprovider.RepairPolicy{
		{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionFalse, TolerationDuration: 10 * time.Minute},
		{ConditionType: corev1.NodeReady, ConditionStatus: corev1.ConditionUnknown, TolerationDuration: 10 * time.Minute},
	}
}

// Name implements cloudprovider.CloudProvider.
func (c *CloudProvider) Name() string { return Name }

// GetSupportedNodeClasses implements cloudprovider.CloudProvider.
func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1beta1.SSHNodeClass{}}
}

// nodeClassByRef resolves an SSHNodeClass from a nodeClassRef (a NodeClaim's
// or a NodePool template's).
func (c *CloudProvider) nodeClassByRef(ctx context.Context, ref *karpv1.NodeClassReference) (*v1beta1.SSHNodeClass, error) {
	if ref == nil {
		return nil, fmt.Errorf("no nodeClassRef")
	}
	nodeClass := &v1beta1.SSHNodeClass{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: ref.Name}, nodeClass); err != nil {
		return nil, err
	}
	return nodeClass, nil
}

func (c *CloudProvider) resolveInstanceTypes(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1beta1.SSHNodeClass) ([]*cloudprovider.InstanceType, error) {
	all, err := c.instanceTypeProvider.List(ctx, nodeClass)
	if err != nil {
		return nil, err
	}
	// karpenter already resolved scheduling constraints into the NodeClaim's
	// requirements; honor the instance-type restriction and stay permissive on
	// well-known labels the pool doesn't define.
	reqs := karpscheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	out := []*cloudprovider.InstanceType{}
	for _, it := range all {
		if reqs.Has(corev1.LabelInstanceTypeStable) && !reqs.Get(corev1.LabelInstanceTypeStable).Has(it.Name) {
			continue
		}
		if err := reqs.Compatible(it.Requirements, karpscheduling.AllowUndefinedWellKnownLabels); err != nil {
			continue
		}
		if !resources.Fits(nodeClaim.Spec.Resources.Requests, it.Allocatable()) {
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

// instanceToNodeClaim maps an instance to karpenter's NodeClaim view. When the
// backing instance type is known (Create path), capacity AND allocatable come
// from it — karpenter core copies both onto the launched NodeClaim and models
// in-flight capacity from them while the SSH join is still running; without
// allocatable (and the pods resource) pending pods never fit the in-flight
// node and core would claim extra hosts. Its single-value requirements become
// labels for the same reason: until the node registers, they are all core knows
// about the arch and OS of what is coming, and pods selecting on them would
// otherwise trigger a second launch.
func (c *CloudProvider) instanceToNodeClaim(inst *instance.Instance, instanceTypes ...*cloudprovider.InstanceType) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: inst.NodeName,
			Labels: map[string]string{
				corev1.LabelInstanceTypeStable: inst.Class,
				corev1.LabelTopologyZone:       instancetype.PoolZone,
				karpv1.CapacityTypeLabelKey:    karpv1.CapacityTypeOnDemand,
			},
			CreationTimestamp: metav1.Time{Time: inst.CreationTime},
		},
	}
	nc.Status.ProviderID = inst.ProviderID
	nc.Status.Capacity = inst.Capacity
	for _, it := range instanceTypes {
		if it.Name == inst.Class {
			nc.Status.Capacity = it.Capacity
			nc.Status.Allocatable = it.Allocatable()
			for key, req := range it.Requirements {
				if req.Len() == 1 {
					nc.Labels[key] = req.Values()[0]
				}
			}
			break
		}
	}
	return nc
}
