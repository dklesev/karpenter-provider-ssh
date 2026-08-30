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

// Package metrics exposes provider-level Prometheus metrics on karpenter's
// metrics endpoint (controller-runtime registry, scraped via the chart's
// ServiceMonitor). Everything here is about the provider's own work — SSH
// probes, join/leave phases, pool inventory — karpenter core publishes its
// scheduling metrics separately.
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/dklesev/karpenter-provider-ssh/pkg/apis/v1beta1"
)

// sshBuckets covers SSH operation latencies: probes land in the sub-second
// range, joins in seconds, installs in minutes.
var sshBuckets = []float64{0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}

var (
	probeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "kpssh",
		Subsystem: "host",
		Name:      "probe_duration_seconds",
		Help:      "Duration of SSH host probes.",
		Buckets:   sshBuckets,
	}, []string{"outcome"})

	phaseDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "kpssh",
		Subsystem: "instance",
		Name:      "phase_duration_seconds",
		Help:      "Duration of profile script phases executed over SSH.",
		Buckets:   sshBuckets,
	}, []string{"phase", "outcome"})

	zombieActions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kpssh",
		Subsystem: "host",
		Name:      "zombie_actions_total",
		Help:      "Zombie guard interventions on unclaimed hosts running a kubelet.",
	}, []string{"action"})
)

func init() {
	crmetrics.Registry.MustRegister(probeDuration, phaseDuration, zombieActions)
}

func outcome(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}

// ObserveProbe records one host probe.
func ObserveProbe(start time.Time, err error) {
	probeDuration.WithLabelValues(outcome(err)).Observe(time.Since(start).Seconds())
}

// ObservePhase records one profile phase execution (install, join, leave).
func ObservePhase(phase string, start time.Time, err error) {
	phaseDuration.WithLabelValues(phase, outcome(err)).Observe(time.Since(start).Seconds())
}

// Zombie guard actions.
const (
	ZombieActionLeft          = "left"
	ZombieActionParkedForeign = "parked_foreign"
	ZombieActionLeaveFailed   = "leave_failed"
)

// RecordZombieAction counts a zombie guard intervention.
func RecordZombieAction(action string) {
	zombieActions.WithLabelValues(action).Inc()
}

// poolCollector reports pool inventory on scrape by listing SSHHosts from the
// controller cache — no background bookkeeping to drift out of sync.
type poolCollector struct {
	client    client.Client
	namespace string
	desc      *prometheus.Desc
}

// NewPoolCollector returns a prometheus collector exposing
// kpssh_pool_hosts{state,class} gauges for the given pool namespace.
func NewPoolCollector(c client.Client, namespace string) prometheus.Collector {
	return &poolCollector{
		client:    c,
		namespace: namespace,
		desc: prometheus.NewDesc(
			"kpssh_pool_hosts",
			"Hosts in the pool by lifecycle state and host class.",
			[]string{"state", "class"}, nil,
		),
	}
}

func (p *poolCollector) Describe(ch chan<- *prometheus.Desc) { ch <- p.desc }

func (p *poolCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hosts := &v1beta1.SSHHostList{}
	if err := p.client.List(ctx, hosts, client.InNamespace(p.namespace)); err != nil {
		// Emit nothing: a gap in the series is honest, a stale value is a lie
		// an alert would act on. But say so — this reads from the informer
		// cache, so it fails only when the cache is unsynced or the context
		// deadline blew, and silently dropping the pool gauge would leave an
		// operator staring at a hole in a dashboard with nothing to grep for.
		ctrllog.Log.WithName("poolcollector").Error(err, "listing hosts for pool metrics; skipping this scrape",
			"namespace", p.namespace)
		return
	}
	type key struct{ state, class string }
	counts := map[key]float64{}
	for i := range hosts.Items {
		h := &hosts.Items[i]
		state := string(h.Status.State)
		if state == "" {
			state = string(v1beta1.HostStatePending)
		}
		counts[key{state, v1beta1.HostClass(h)}]++
	}
	for k, v := range counts {
		ch <- prometheus.MustNewConstMetric(p.desc, prometheus.GaugeValue, v, k.state, k.class)
	}
}
