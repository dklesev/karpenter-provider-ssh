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

// Package e2e is the full-loop end-to-end suite: a kind control plane plus
// privileged systemd containers standing in for SSH pool hosts, exercising the
// real join profiles and the whole claim -> join -> consolidate -> warm-release
// loop, including verified execution.
//
// Each scenario runs in its OWN ephemeral kind cluster (karpenter core is
// cluster-global — one consolidation/disruption loop, one singleton controller
// — so scenarios needing conflicting NodePool disruption settings, or that
// force-delete NodeClaims, cannot share a cluster without serialising or
// cross-contaminating). Go owns the parallelism (t.Parallel()), the per-cluster
// lifecycle (t.Cleanup) and the assertions (a typed client against the
// cluster). The two expensive, read-only artifacts — the controller image and
// the pool-host image — are built ONCE in TestMain and `kind load`ed into each
// throwaway cluster.
//
// The whole suite is gated behind the `e2e` build tag so `go test ./...` never
// spins up kind. This file carries no build tag on purpose: it keeps the
// package non-empty for the default (untagged) build, so `go test ./...`
// reports "no test files" here instead of failing on "build constraints
// exclude all Go files".
//
//	go test -tags e2e -timeout 40m -parallel 3 ./test/e2e    # or: make e2e
package e2e
