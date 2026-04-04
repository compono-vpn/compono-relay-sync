package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// syncMetrics holds all Prometheus metrics for relay-sync.
type syncMetrics struct {
	syncRunsTotal       *prometheus.CounterVec
	syncDuration        *prometheus.HistogramVec
	consecutiveFailures *prometheus.GaugeVec
}

func newSyncMetrics() *syncMetrics {
	return &syncMetrics{
		syncRunsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "relay_sync_runs_total",
			Help: "Total number of sync runs per relay and status",
		}, []string{"relay", "status"}),

		syncDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "relay_sync_duration_seconds",
			Help:    "Duration of sync operations per relay",
			Buckets: prometheus.DefBuckets,
		}, []string{"relay"}),

		consecutiveFailures: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "relay_consecutive_failures",
			Help: "Current consecutive failure count per relay",
		}, []string{"relay"}),
	}
}

func (m *syncMetrics) recordRun(relay, status string, durationSec float64, consecFails int) {
	m.syncRunsTotal.WithLabelValues(relay, status).Inc()
	m.syncDuration.WithLabelValues(relay).Observe(durationSec)
	m.consecutiveFailures.WithLabelValues(relay).Set(float64(consecFails))
}
