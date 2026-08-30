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

package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

func poolHost(name, ns string, state v1beta1.HostState, class string) *v1beta1.SSHHost {
	h := &v1beta1.SSHHost{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1beta1.SSHHostSpec{
			Address:         "10.0.0.1",
			SSHKeySecretRef: corev1.LocalObjectReference{Name: "k"},
		},
		Status: v1beta1.SSHHostStatus{State: state},
	}
	if class != "" {
		h.Labels = map[string]string{v1beta1.HostClassLabel: class}
	}
	return h
}

func TestPoolCollector(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(
		poolHost("a", "pool", v1beta1.HostStateAvailable, "bm-large"),
		poolHost("b", "pool", v1beta1.HostStateAvailable, "bm-large"),
		poolHost("c", "pool", v1beta1.HostStateInUse, "bm-large"),
		poolHost("d", "pool", "", ""),                                     // unset state → Pending, class → default
		poolHost("e", "other-ns", v1beta1.HostStateAvailable, "bm-large"), // outside pool namespace → excluded
	).Build()

	col := NewPoolCollector(c, "pool")

	if problems, err := testutil.CollectAndLint(col); err != nil || len(problems) > 0 {
		t.Fatalf("lint: %v problems=%v", err, problems)
	}

	want := `
# HELP kpssh_pool_hosts Hosts in the pool by lifecycle state and host class.
# TYPE kpssh_pool_hosts gauge
kpssh_pool_hosts{class="bm-large",state="Available"} 2
kpssh_pool_hosts{class="bm-large",state="InUse"} 1
kpssh_pool_hosts{class="default",state="Pending"} 1
`
	if err := testutil.CollectAndCompare(col, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected metrics:\n%v", err)
	}
}

func TestPoolCollectorEmptyPool(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).Build()
	col := NewPoolCollector(c, "pool")
	if n := testutil.CollectAndCount(col); n != 0 {
		t.Fatalf("metrics on empty pool = %d, want 0", n)
	}
}

func TestPoolCollectorListErrorSkipsScrape(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return errors.New("cache not synced")
			},
		}).Build()
	col := NewPoolCollector(c, "pool")
	// A failing List must produce a scrape gap (zero metrics), not a panic.
	if n := testutil.CollectAndCount(col); n != 0 {
		t.Fatalf("metrics on list error = %d, want 0", n)
	}
}
