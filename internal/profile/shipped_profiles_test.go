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

package profile

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"

	infrav1 "github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

// The profiles we ship are the first thing an operator applies — they must
// satisfy the same contract the nodeclass controller enforces (parseable
// scripts, and a leave that renders without a node class).
func TestShippedProfilesValidate(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "profile-*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no example profiles found — did they move?")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			p := &infrav1.SSHJoinProfile{}
			if err := yaml.Unmarshal(raw, p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := Validate(p); err != nil {
				t.Fatalf("shipped profile is invalid: %v", err)
			}
		})
	}
}
