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

// Exit-node sync drift metrics (step-1 observer).
// Target label = node name ("FI-HEL-Exit-01"), tag = inbound tag.
// Cardinality stays bounded: ~4-8 exits × ~1-2 inbounds per exit.
type exitMetrics struct {
	expectedUsers    *prometheus.GaugeVec
	actualUsers      *prometheus.GaugeVec
	missingUsers     *prometheus.GaugeVec
	staleUsers       *prometheus.GaugeVec
	lastSuccess      *prometheus.GaugeVec
	observerErrors   *prometheus.CounterVec
	reconcileCalls   *prometheus.CounterVec
	reconcileAdded   *prometheus.CounterVec
	reconcileRemoved *prometheus.CounterVec
}

func newExitMetrics() *exitMetrics {
	return &exitMetrics{
		expectedUsers: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "compono_sync_expected_users",
			Help: "Active panel users that should be provisioned on target for this inbound tag",
		}, []string{"target", "tag"}),

		actualUsers: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "compono_sync_actual_users",
			Help: "Users currently in xray on target for this inbound tag",
		}, []string{"target", "tag"}),

		missingUsers: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "compono_sync_missing_users",
			Help: "Active panel users absent from target's xray for this inbound tag (BDT-27 new-user breakage)",
		}, []string{"target", "tag"}),

		staleUsers: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "compono_sync_stale_users",
			Help: "Users on target's xray that are no longer ACTIVE in panel (BDT-27 undefined-payload remove failure)",
		}, []string{"target", "tag"}),

		lastSuccess: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "compono_sync_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last successful observation of this target",
		}, []string{"target"}),

		observerErrors: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "compono_sync_observer_errors_total",
			Help: "Errors encountered by the exit-node observer, labelled by phase (fetch_nodes, fetch_expected, fetch_actual, reconcile)",
		}, []string{"phase"}),

		// Step-2: feature-flagged writer call into compono-backend's
		// /api/nodes/:uuid/reconcile-users.
		reconcileCalls: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "compono_sync_reconcile_calls_total",
			Help: "POST /api/nodes/:uuid/reconcile-users invocations from the observer, by target and result (ok | error | skipped)",
		}, []string{"target", "result"}),

		reconcileAdded: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "compono_sync_reconcile_added_total",
			Help: "Users added to target's xray by the observer's reconcile call",
		}, []string{"target"}),

		reconcileRemoved: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "compono_sync_reconcile_removed_total",
			Help: "Users removed from target's xray by the observer's reconcile call",
		}, []string{"target"}),
	}
}
