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
	"github.com/awslabs/operatorpkg/controller"
	"github.com/awslabs/operatorpkg/status"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
	"github.com/dklesev/karpenter-provider-ssh/pkg/providers/host"
)

// NewControllers returns the provider-side reconcilers. The operatorpkg
// status controller publishes operator_status_condition_* metrics and
// transition events for SSHNodeClass Ready — same observability contract as
// karpenter core's own kinds.
func NewControllers(kubeClient client.Client, kubernetesInterface kubernetes.Interface, hostProvider host.Provider, eventRecorder record.EventRecorder) []controller.Controller {
	return []controller.Controller{
		// Exec/ExecShim/Recorder/Bootstrap are defaulted in Register.
		&HostProbeReconciler{Client: kubeClient, KubernetesInterface: kubernetesInterface},
		&NodeClassReconciler{Client: kubeClient, HostProvider: hostProvider},
		status.NewController[*v1beta1.SSHNodeClass](kubeClient, eventRecorder),
	}
}
