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
	"errors"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
)

// createFailedEvent surfaces why a NodeClaim could not be turned into a
// joined host (claim, install/join over SSH, providerID adoption).
func createFailedEvent(nodeClaim *karpv1.NodeClaim, err error) events.Event {
	reason := "CreateFailed"
	var createErr *cloudprovider.CreateError
	if errors.As(err, &createErr) {
		reason = createErr.ConditionReason
	}
	return events.Event{
		InvolvedObject: nodeClaim,
		Type:           corev1.EventTypeWarning,
		Reason:         reason,
		Message:        err.Error(),
		DedupeValues:   []string{nodeClaim.Name, reason},
	}
}
