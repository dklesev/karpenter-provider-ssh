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

// Package main runs karpenter core with the SSH pool cloud provider.
package main

import (
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	corecontrollers "sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"

	sshcloudprovider "github.com/dklesev/karpenter-provider-ssh/pkg/cloudprovider"
	"github.com/dklesev/karpenter-provider-ssh/pkg/controllers"
	"github.com/dklesev/karpenter-provider-ssh/pkg/operator"
)

func main() {
	ctx, op := operator.NewOperator(coreoperator.NewOperator())
	log := ctrl.Log.WithName("setup")
	log.Info("karpenter-provider-ssh", "version", coreoperator.Version)

	sshCloudProvider := sshcloudprovider.New(
		op.GetClient(),
		op.EventRecorder,
		op.InstanceProvider,
		op.InstanceTypeProvider,
	)
	cloudProvider := metrics.Decorate(sshCloudProvider)
	clusterState := state.NewCluster(op.Clock, op.GetClient(), cloudProvider)

	op.
		WithControllers(ctx, corecontrollers.NewControllers(
			ctx,
			op.Manager,
			op.Clock,
			op.GetClient(),
			op.EventRecorder,
			cloudProvider,
			sshCloudProvider,
			clusterState,
			op.InstanceTypeStore,
		)...).
		WithControllers(ctx, controllers.NewControllers(
			op.GetClient(), op.KubernetesInterface, op.HostProvider,
			// operatorpkg's status controller still takes the legacy
			// record.EventRecorder; switch to GetEventRecorder once it
			// migrates to the events.k8s.io API. The SA1019 deprecation is
			// suppressed in .golangci.yml (an inline //nolint here flaps with
			// a warm lint cache).
			op.Manager.GetEventRecorderFor("kpssh-nodeclass-status"))...).
		Start(ctx)
}
