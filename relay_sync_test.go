package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestBackoff_ConsecutiveFailuresIncreaseDelay(t *testing.T) {
	tracker := newRelayTracker([]string{"relay-a"})

	now := time.Now()
	tracker.recordFailure("relay-a", now, "connection refused")
	if tracker.consecutiveFailures("relay-a") != 1 {
		t.Fatalf("expected 1 consecutive failure, got %d", tracker.consecutiveFailures("relay-a"))
	}

	// After 1 failure, backoff is 2s. Checking 1s later should skip.
	if !tracker.shouldSkip("relay-a", now.Add(1*time.Second)) {
		t.Fatal("expected relay to be in backoff 1s after first failure")
	}

	// After backoff expires (2s), should NOT skip.
	if tracker.shouldSkip("relay-a", now.Add(3*time.Second)) {
		t.Fatal("expected relay to not be in backoff after 3s")
	}

	// Record more failures; backoff should grow.
	tracker.recordFailure("relay-a", now.Add(3*time.Second), "timeout")
	tracker.recordFailure("relay-a", now.Add(6*time.Second), "timeout")
	if tracker.consecutiveFailures("relay-a") != 3 {
		t.Fatalf("expected 3 consecutive failures, got %d", tracker.consecutiveFailures("relay-a"))
	}

	// After 3 failures, backoff = 2s * 2^2 = 8s from last attempt.
	lastAttempt := now.Add(6 * time.Second)
	if !tracker.shouldSkip("relay-a", lastAttempt.Add(5*time.Second)) {
		t.Fatal("expected relay to be in backoff 5s after 3rd failure (backoff=8s)")
	}
	if tracker.shouldSkip("relay-a", lastAttempt.Add(9*time.Second)) {
		t.Fatal("expected relay to not be in backoff 9s after 3rd failure (backoff=8s)")
	}
}

func TestBackoff_SuccessClearsFailureStreak(t *testing.T) {
	tracker := newRelayTracker([]string{"relay-a"})

	now := time.Now()
	// Accumulate failures.
	tracker.recordFailure("relay-a", now, "err1")
	tracker.recordFailure("relay-a", now.Add(1*time.Second), "err2")
	tracker.recordFailure("relay-a", now.Add(2*time.Second), "err3")
	if tracker.consecutiveFailures("relay-a") != 3 {
		t.Fatalf("expected 3 failures, got %d", tracker.consecutiveFailures("relay-a"))
	}

	// Success clears everything.
	tracker.recordSuccess("relay-a", now.Add(10*time.Second), 42, true)
	if tracker.consecutiveFailures("relay-a") != 0 {
		t.Fatalf("expected 0 failures after success, got %d", tracker.consecutiveFailures("relay-a"))
	}

	// Should not skip after success.
	if tracker.shouldSkip("relay-a", now.Add(10*time.Second)) {
		t.Fatal("expected no backoff after successful sync")
	}
}

func TestBackoff_CappedAt60s(t *testing.T) {
	tracker := newRelayTracker([]string{"relay-a"})

	now := time.Now()
	// Record many failures to exceed the cap.
	for i := range 20 {
		tracker.recordFailure("relay-a", now.Add(time.Duration(i)*time.Minute), "err")
	}

	snap := tracker.snapshot()
	delay := snap["relay-a"].backoffDelay()
	if delay != backoffMax {
		t.Fatalf("expected backoff capped at %v, got %v", backoffMax, delay)
	}
}

func TestManualTrigger_BypassesBackoff(t *testing.T) {
	// Set up a mock API server that returns UUIDs.
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"response":{"uuids":["uuid-1","uuid-2"]}}`)
	}))
	defer apiServer.Close()

	// Set up a relay server that always fails.
	failRelay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"status":"error","error":"disk full"}`)
	}))
	defer failRelay.Close()

	cfg := config{
		apiURL:       apiServer.URL,
		apiToken:     "test-token",
		relayURLs:    []string{failRelay.URL},
		relayTokens:  []string{"relay-token"},
		syncInterval: time.Minute,
	}
	tracker := newRelayTracker(cfg.relayURLs)
	metrics := newTestMetrics(t)

	// Cause failures to enter backoff by using bypass mode (simulating manual retries).
	// Periodic mode would skip after the first failure due to backoff.
	for range 5 {
		_, _ = runSync(cfg, tracker, metrics, true)
	}

	if tracker.consecutiveFailures(failRelay.URL) != 5 {
		t.Fatalf("expected 5 failures, got %d", tracker.consecutiveFailures(failRelay.URL))
	}

	// Periodic sync should skip due to backoff.
	result, err := runSync(cfg, tracker, metrics, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	foundSkipped := false
	for _, r := range result.Relays {
		if r.Relay == failRelay.URL && r.Skipped {
			foundSkipped = true
		}
	}
	if !foundSkipped {
		t.Fatal("expected relay to be skipped during periodic sync due to backoff")
	}

	// Manual trigger (bypassBackoff=true) should NOT skip.
	result, err = runSync(cfg, tracker, metrics, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, r := range result.Relays {
		if r.Relay == failRelay.URL && r.Skipped {
			t.Fatal("expected relay NOT to be skipped during manual trigger")
		}
	}
}

func TestHealthEndpoint_MixedState(t *testing.T) {
	tracker := newRelayTracker([]string{"relay-healthy", "relay-failing"})

	now := time.Now()
	// Healthy relay had a recent success.
	tracker.recordSuccess("relay-healthy", now, 100, false)

	// Failing relay has many consecutive failures.
	for i := range degradedThreshold + 1 {
		tracker.recordFailure("relay-failing", now.Add(time.Duration(i)*time.Second), "timeout")
	}

	// Build the health handler inline (same logic as main).
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		snap := tracker.snapshot()
		var degraded []string
		for relay, state := range snap {
			if state.ConsecFails >= degradedThreshold {
				degraded = append(degraded, relay)
			}
		}
		status := "ok"
		if len(degraded) > 0 {
			status = "degraded"
		}
		_ = json.NewEncoder(w).Encode(healthResponse{
			Status:       status,
			RelayCount:   2,
			DegradedList: degraded,
		})
	})

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp healthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode health response: %v", err)
	}

	if resp.Status != "degraded" {
		t.Fatalf("expected degraded status, got %q", resp.Status)
	}

	if len(resp.DegradedList) != 1 || resp.DegradedList[0] != "relay-failing" {
		t.Fatalf("expected [relay-failing] in degraded list, got %v", resp.DegradedList)
	}
}

func TestStatusEndpoint_ReturnsPerRelayState(t *testing.T) {
	tracker := newRelayTracker([]string{"relay-a", "relay-b"})

	now := time.Now()
	tracker.recordSuccess("relay-a", now, 50, true)
	tracker.recordFailure("relay-b", now, "connection reset")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tracker.snapshot())
	})

	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var states map[string]relayState
	if err := json.Unmarshal(w.Body.Bytes(), &states); err != nil {
		t.Fatalf("failed to decode status response: %v", err)
	}

	a, ok := states["relay-a"]
	if !ok {
		t.Fatal("relay-a not found in status")
	}
	if a.ConsecFails != 0 || a.LastUserCount != 50 || !a.LastChanged {
		t.Fatalf("unexpected state for relay-a: %+v", a)
	}

	b, ok := states["relay-b"]
	if !ok {
		t.Fatal("relay-b not found in status")
	}
	if b.ConsecFails != 1 || b.LastError != "connection reset" {
		t.Fatalf("unexpected state for relay-b: %+v", b)
	}
}

func TestMetrics_Registration(t *testing.T) {
	// Use a fresh registry to avoid conflicts with default registry in other tests.
	reg := prometheus.NewRegistry()

	syncRuns := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_relay_sync_runs_total",
		Help: "test",
	}, []string{"relay", "status"})

	syncDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "test_relay_sync_duration_seconds",
		Help: "test",
	}, []string{"relay"})

	consecFails := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "test_relay_consecutive_failures",
		Help: "test",
	}, []string{"relay"})

	if err := reg.Register(syncRuns); err != nil {
		t.Fatalf("failed to register sync_runs: %v", err)
	}
	if err := reg.Register(syncDuration); err != nil {
		t.Fatalf("failed to register sync_duration: %v", err)
	}
	if err := reg.Register(consecFails); err != nil {
		t.Fatalf("failed to register consec_fails: %v", err)
	}

	// Record a metric and verify it works.
	syncRuns.WithLabelValues("relay-test", "success").Inc()
	syncDuration.WithLabelValues("relay-test").Observe(0.5)
	consecFails.WithLabelValues("relay-test").Set(3)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	if len(mfs) != 3 {
		t.Fatalf("expected 3 metric families, got %d", len(mfs))
	}
}

// newTestMetrics creates a syncMetrics instance for testing.
// It uses promauto with the default registry which is shared across tests,
// but since metric names are the same, subsequent registrations are fine
// (promauto handles this).
func newTestMetrics(t *testing.T) *syncMetrics {
	t.Helper()
	return &syncMetrics{
		syncRunsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "test_sync_runs_total",
			Help: "test",
		}, []string{"relay", "status"}),
		syncDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "test_sync_duration_seconds",
			Help: "test",
		}, []string{"relay"}),
		consecutiveFailures: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "test_consecutive_failures",
			Help: "test",
		}, []string{"relay"}),
	}
}
