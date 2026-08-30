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

// Package operator assembles the provider on top of karpenter's core operator.
package operator

import (
	"context"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/karpenter/pkg/operator"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/metrics"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/bootstrap"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/host"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/instance"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/instancetype"
	"k8s.io/utils/env"
)

// Operator wraps karpenter's operator with the SSH pool providers.
type Operator struct {
	*operator.Operator

	HostProvider         host.Provider
	InstanceProvider     instance.Provider
	InstanceTypeProvider instancetype.Provider
}

// RegisterFieldIndexes registers the provider's field indexes on a manager's
// indexer. It must run before the cache starts.
//
// Exported because anything that builds a manager for these controllers needs
// the identical set — the integration suite included. A missing index is not a
// compile error: the List simply fails at runtime ("no index with name … has
// been registered"), the probe wedges, and the host never leaves Pending. That
// is a bad way to find out, so there is exactly one definition of the set.
//
// Two hot paths resolve a host to its Node by InternalIP: providerID adoption
// (polls every few seconds for minutes, per launch, on the EKS Hybrid path)
// and the probe's zombie guard (per host, per interval). Unindexed, each of
// those deep-copies every Node in the cluster.
func RegisterFieldIndexes(ctx context.Context, fi client.FieldIndexer) error {
	return fi.IndexField(ctx, &corev1.Node{}, v1beta1.NodeInternalIPIndex,
		func(o client.Object) []string { return v1beta1.NodeInternalIPs(o.(*corev1.Node)) })
}

// NewOperator builds all providers. POOL_NAMESPACE scopes the SSHHost
// inventory (default: kpssh-system).
func NewOperator(ctx context.Context, op *operator.Operator) (context.Context, *Operator) {
	poolNamespace := env.GetString("POOL_NAMESPACE", "kpssh-system")
	log.FromContext(ctx).Info("initializing karpenter-provider-ssh", "poolNamespace", poolNamespace)

	lo.Must0(RegisterFieldIndexes(ctx, op.GetFieldIndexer()))

	hostProvider := host.NewDefaultProvider(op.GetClient(), poolNamespace)
	bootstrapProvider := bootstrap.NewDefaultProvider(op.KubernetesInterface)
	instanceTypeProvider := instancetype.NewDefaultProvider(hostProvider)
	instanceProvider := instance.NewDefaultProvider(op.GetClient(), op.KubernetesInterface, hostProvider, bootstrapProvider)

	crmetrics.Registry.MustRegister(metrics.NewPoolCollector(op.GetClient(), poolNamespace))

	return ctx, &Operator{
		Operator:             op,
		HostProvider:         hostProvider,
		InstanceProvider:     instanceProvider,
		InstanceTypeProvider: instanceTypeProvider,
	}
}
