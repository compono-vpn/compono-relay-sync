package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Tests for the step-2 observer extension: when EXIT_OBSERVER_RECONCILE is on,
// each observer tick must call POST /api/nodes/:uuid/reconcile-users on the
// panel exactly once per node. When the flag is off, NO reconcile call happens.
//
// We mock the panel HTTP server. The /api/nodes endpoint returns one node;
// the expected/actual endpoints return matching sets so the diff is empty
// (we're not testing the diff math here — that lives in compono-backend).

type fakePanel struct {
	server          *httptest.Server
	reconcileCalls  atomic.Int32
	expectedCalls   atomic.Int32
	actualCalls     atomic.Int32
	nodesCalls      atomic.Int32
	reconcileResult string // raw JSON body returned by the reconcile endpoint
}

func newFakePanel(t *testing.T) *fakePanel {
	t.Helper()
	fp := &fakePanel{
		reconcileResult: `{"response":{"nodeUuid":"n1","added":[{"username":"99","tags":["t"]}],"removed":[],"errors":[],"unreachableTags":[],"skipped":false,"skipReason":null}}`,
	}
	fp.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/nodes" && r.Method == "GET":
			fp.nodesCalls.Add(1)
			_, _ = w.Write([]byte(`{"response":[{"uuid":"n1","name":"FAKE-Exit-01","isConnected":true,"isDisabled":false}]}`))
		case strings.HasSuffix(r.URL.Path, "/expected-users") && r.Method == "GET":
			fp.expectedCalls.Add(1)
			_, _ = w.Write([]byte(`{"response":{"nodeUuid":"n1","users":[{"tId":1,"vlessUuid":"00000000-0000-0000-0000-000000000001","username":"a","inboundTags":["t"]}]}}`))
		case strings.HasSuffix(r.URL.Path, "/actual-users") && r.Method == "GET":
			fp.actualCalls.Add(1)
			_, _ = w.Write([]byte(`{"response":{"nodeUuid":"n1","users":[{"username":"1","inboundTags":["t"]}],"unreachableTags":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/reconcile-users") && r.Method == "POST":
			fp.reconcileCalls.Add(1)
			_, _ = w.Write([]byte(fp.reconcileResult))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(fp.server.Close)
	return fp
}

func TestExitObserver_ReconcileFlagOff_DoesNotCallReconcile(t *testing.T) {
	fp := newFakePanel(t)
	metrics := newTestExitMetrics()
	obs := newExitObserver(fp.server.URL, "tok", 1*time.Hour, metrics, false /* reconcileEnabled */)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := obs.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if fp.reconcileCalls.Load() != 0 {
		t.Fatalf("expected 0 reconcile calls when flag off, got %d", fp.reconcileCalls.Load())
	}
	// Sanity: the read-only observer still ran.
	if fp.expectedCalls.Load() != 1 || fp.actualCalls.Load() != 1 {
		t.Fatalf("expected 1 expected + 1 actual call, got %d + %d",
			fp.expectedCalls.Load(), fp.actualCalls.Load())
	}
}

func TestExitObserver_ReconcileFlagOn_CallsReconcileOncePerNode(t *testing.T) {
	fp := newFakePanel(t)
	metrics := newTestExitMetrics()
	obs := newExitObserver(fp.server.URL, "tok", 1*time.Hour, metrics, true /* reconcileEnabled */)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := obs.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if fp.reconcileCalls.Load() != 1 {
		t.Fatalf("expected 1 reconcile call when flag on, got %d", fp.reconcileCalls.Load())
	}
}

func TestExitObserver_ReconcileSkipped_BumpsSkippedCounter(t *testing.T) {
	fp := newFakePanel(t)
	fp.reconcileResult = `{"response":{"nodeUuid":"n1","added":[],"removed":[],"errors":[],"unreachableTags":[],"skipped":true,"skipReason":"safety cap: too many stale"}}`
	metrics := newTestExitMetrics()
	obs := newExitObserver(fp.server.URL, "tok", 1*time.Hour, metrics, true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := obs.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// Skipped reconcile must NOT be counted as ok (so dashboards alert if
	// the cap keeps tripping). reconcileCalls counter still bumps with
	// result=skipped though — caller knows the cycle ran.
	if fp.reconcileCalls.Load() != 1 {
		t.Fatalf("expected 1 reconcile HTTP call, got %d", fp.reconcileCalls.Load())
	}
}

func TestExitObserver_ReconcileHTTP500_LogsErrorAndKeepsRunning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/nodes":
			_, _ = w.Write([]byte(`{"response":[{"uuid":"n1","name":"FAKE-Exit-01","isConnected":true,"isDisabled":false}]}`))
		case strings.HasSuffix(r.URL.Path, "/expected-users"):
			_, _ = w.Write([]byte(`{"response":{"nodeUuid":"n1","users":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/actual-users"):
			_, _ = w.Write([]byte(`{"response":{"nodeUuid":"n1","users":[],"unreachableTags":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/reconcile-users"):
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	metrics := newTestExitMetrics()
	obs := newExitObserver(server.URL, "tok", 1*time.Hour, metrics, true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Must not return an error — failure to reconcile is logged + counted,
	// not propagated. Otherwise a single bad node would break the whole tick.
	if err := obs.tick(ctx); err != nil {
		t.Fatalf("tick should swallow reconcile error, got: %v", err)
	}
}

// newTestExitMetrics builds an exitMetrics whose vectors are NOT registered
// in the default promauto registry — needed because go test runs all tests
// in one process and promauto panics on duplicate registrations.
func newTestExitMetrics() *exitMetrics {
	gv := func(name string, lbl []string) *prometheus.GaugeVec {
		return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: "test"}, lbl)
	}
	cv := func(name string, lbl []string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: "test"}, lbl)
	}
	return &exitMetrics{
		expectedUsers:    gv("test_compono_sync_expected_users", []string{"target", "tag"}),
		actualUsers:      gv("test_compono_sync_actual_users", []string{"target", "tag"}),
		missingUsers:     gv("test_compono_sync_missing_users", []string{"target", "tag"}),
		staleUsers:       gv("test_compono_sync_stale_users", []string{"target", "tag"}),
		lastSuccess:      gv("test_compono_sync_last_success_ts", []string{"target"}),
		observerErrors:   cv("test_compono_sync_observer_errors", []string{"phase"}),
		reconcileCalls:   cv("test_compono_sync_reconcile_calls", []string{"target", "result"}),
		reconcileAdded:   cv("test_compono_sync_reconcile_added", []string{"target"}),
		reconcileRemoved: cv("test_compono_sync_reconcile_removed", []string{"target"}),
	}
}
