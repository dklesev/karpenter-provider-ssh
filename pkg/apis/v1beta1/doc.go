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

// Package v1beta1 contains the karpenter-provider-ssh API types:
// SSHHost (pool inventory), SSHNodeClass (karpenter node class), and
// SSHJoinProfile (pluggable install/join/leave/uninstall scripts).
// +kubebuilder:object:generate=true
// +groupName=karpenter.dklesev.github.io
package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "karpenter.dklesev.github.io", Version: "v1beta1"}

	// SchemeBuilder collects the functions that register this group's types
	// into a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&SSHHost{}, &SSHHostList{},
		&SSHNodeClass{}, &SSHNodeClassList{},
		&SSHJoinProfile{}, &SSHJoinProfileList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// karpenter's operator builds its manager on the global client-go scheme, so
// the provider types must be registered there (same pattern as other
// karpenter providers).
func init() {
	utilruntime.Must(AddToScheme(clientgoscheme.Scheme))
}
