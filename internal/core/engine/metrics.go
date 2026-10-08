package engine

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the engine's Prometheus metrics.
type Metrics struct {
	events        *prometheus.CounterVec
	commits       *prometheus.CounterVec
	commitSeconds *prometheus.HistogramVec
	mergeSeconds  *prometheus.HistogramVec
	batchRows     *prometheus.HistogramVec
	lag           *prometheus.GaugeVec
	errors        *prometheus.CounterVec
	runners       prometheus.Gauge
	snapshotDone  *prometheus.GaugeVec
	maintenance   *prometheus.CounterVec
}

// NewMetrics registers the metrics on reg (default registry when nil).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	labels := []string{"pipeline", "collection"}
	m := &Metrics{
		events:        prometheus.NewCounterVec(prometheus.CounterOpts{Name: "lakesync_events_total", Help: "Change events written to raw tables."}, append(labels, "op")),
		commits:       prometheus.NewCounterVec(prometheus.CounterOpts{Name: "lakesync_commits_total", Help: "Raw table commits."}, append(labels, "result")),
		commitSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "lakesync_commit_seconds", Help: "Time to flush a batch.", Buckets: prometheus.ExponentialBuckets(0.05, 2, 12)}, labels),
		mergeSeconds:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "lakesync_merge_seconds", Help: "Time to merge raw into clean.", Buckets: prometheus.ExponentialBuckets(0.05, 2, 12)}, labels),
		batchRows:     prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "lakesync_batch_rows", Help: "Rows per flushed batch.", Buckets: prometheus.ExponentialBuckets(1, 4, 10)}, labels),
		lag:           prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "lakesync_lag_seconds", Help: "Now minus source time of the last applied event."}, labels),
		errors:        prometheus.NewCounterVec(prometheus.CounterOpts{Name: "lakesync_errors_total", Help: "Errors by kind."}, append(labels, "kind")),
		runners:       prometheus.NewGauge(prometheus.GaugeOpts{Name: "lakesync_runners", Help: "Active collection runners in this process."}),
		snapshotDone:  prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "lakesync_snapshot_done", Help: "Documents copied in the current full copy."}, labels),
		maintenance:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "lakesync_maintenance_total", Help: "Maintenance jobs run."}, []string{"kind", "result"}),
	}
	for _, c := range []prometheus.Collector{m.events, m.commits, m.commitSeconds, m.mergeSeconds, m.batchRows, m.lag, m.errors, m.runners, m.snapshotDone, m.maintenance} {
		if err := reg.Register(c); err != nil {
			if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
				_ = are
				continue
			}
		}
	}
	return m
}
