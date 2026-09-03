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
	"testing"
)

// FuzzParseProbeOutput: the probe script's stdout is produced on a pool host
// the operator, not the controller, administers. Whatever comes back, the
// parser must return a result carrying the caller's fingerprint (the host-key
// pin is decided from it) and an arch that is already normalized — a raw
// uname value leaking through would mismatch every node-selector.
func FuzzParseProbeOutput(f *testing.F) {
	f.Add("4 16384000 x86_64 active\n")
	f.Add("2 4096 aarch64 inactive")
	f.Add("8 32768000 arm64")
	f.Add("")
	f.Add("-1 99999999999999999999 riscv64 active extra fields")
	f.Add("\t\n \n")

	f.Fuzz(func(t *testing.T, stdout string) {
		res := parseProbeOutput(stdout, "SHA256:x")
		if res == nil {
			t.Fatal("nil result")
		}
		if res.fingerprint != "SHA256:x" {
			t.Fatalf("fingerprint %q not preserved", res.fingerprint)
		}
		if res.arch == "x86_64" || res.arch == "aarch64" {
			t.Fatalf("arch %q not normalized", res.arch)
		}
	})
}
