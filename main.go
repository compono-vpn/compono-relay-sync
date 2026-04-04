package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type config struct {
	apiURL       string
	apiToken     string
	relayURLs    []string
	relayTokens  []string
	listenAddr   string
	syncInterval time.Duration
}

type uuidsResponse struct {
	Response struct {
		UUIDs []string `json:"uuids"`
	} `json:"response"`
}

type syncRequest struct {
	UUIDs []string `json:"uuids"`
}

type syncResponse struct {
	Status    string `json:"status"`
	Changed   bool   `json:"changed"`
	UserCount int    `json:"user_count"`
	Error     string `json:"error,omitempty"`
}

type relayResult struct {
	Relay     string `json:"relay"`
	Status    string `json:"status"`
	Changed   bool   `json:"changed"`
	UserCount int    `json:"user_count"`
	Error     string `json:"error,omitempty"`
	Skipped   bool   `json:"skipped,omitempty"`
}

type triggerResponse struct {
	Status   string        `json:"status"`
	UUIDs    int           `json:"uuids"`
	Relays   []relayResult `json:"relays"`
	Duration string        `json:"duration"`
}

type healthResponse struct {
	Status       string   `json:"status"`
	RelayCount   int      `json:"relay_count"`
	DegradedList []string `json:"degraded_relays,omitempty"`
}

var syncMu sync.Mutex

func main() {
	cfg := mustLoadConfig()
	tracker := newRelayTracker(cfg.relayURLs)
	metrics := newSyncMetrics()

	log.Printf("relay-sync: api=%s relays=%d listen=%s interval=%s",
		cfg.apiURL, len(cfg.relayURLs), cfg.listenAddr, cfg.syncInterval)

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
		slices.Sort(degraded)

		status := "ok"
		if len(degraded) > 0 {
			status = "degraded"
		}

		resp := healthResponse{
			Status:       status,
			RelayCount:   len(cfg.relayURLs),
			DegradedList: degraded,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		snap := tracker.snapshot()
		_ = json.NewEncoder(w).Encode(snap)
	})

	mux.HandleFunc("POST /trigger", func(w http.ResponseWriter, r *http.Request) {
		// Manual trigger bypasses backoff.
		result, err := runSync(cfg, tracker, metrics, true)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "error",
				"error":  err.Error(),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	})

	mux.Handle("GET /metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:    cfg.listenAddr,
		Handler: mux,
	}

	// Start periodic sync goroutine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		// Run an initial sync on startup
		log.Println("running initial sync")
		if _, err := runSync(cfg, tracker, metrics, false); err != nil {
			log.Printf("initial sync error: %v", err)
		}

		ticker := time.NewTicker(cfg.syncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := runSync(cfg, tracker, metrics, false); err != nil {
					log.Printf("periodic sync error: %v", err)
				}
			}
		}
	}()

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh
		log.Printf("received %v, shutting down", sig)
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	log.Printf("listening on %s", cfg.listenAddr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
	log.Println("server stopped")
}

func runSync(cfg config, tracker *relayTracker, metrics *syncMetrics, bypassBackoff bool) (*triggerResponse, error) {
	syncMu.Lock()
	defer syncMu.Unlock()

	start := time.Now()

	uuids, err := fetchActiveUUIDs(cfg)
	if err != nil {
		return nil, fmt.Errorf("fetch UUIDs: %w", err)
	}

	slices.Sort(uuids)

	resultsCh := make(chan relayResult, len(cfg.relayURLs))
	var wg sync.WaitGroup

	for i, relayURL := range cfg.relayURLs {
		token := cfg.relayTokens[i]

		// Check backoff unless this is a manual trigger.
		if !bypassBackoff && tracker.shouldSkip(relayURL, time.Now()) {
			log.Printf("relay %s: skipped (backoff, %d consecutive failures)",
				relayURL, tracker.consecutiveFailures(relayURL))
			resultsCh <- relayResult{
				Relay:   relayURL,
				Status:  "skipped",
				Skipped: true,
			}
			continue
		}

		wg.Add(1)
		go func(relayURL, token string) {
			defer wg.Done()
			syncURL := strings.TrimRight(relayURL, "/") + "/sync"
			relayStart := time.Now()

			resp, syncErr := pushToRelay(syncURL, token, uuids)
			duration := time.Since(relayStart).Seconds()
			now := time.Now()

			if syncErr != nil {
				log.Printf("relay %s: ERROR: %v", relayURL, syncErr)
				tracker.recordFailure(relayURL, now, syncErr.Error())
				consecFails := tracker.consecutiveFailures(relayURL)
				metrics.recordRun(relayURL, "error", duration, consecFails)
				resultsCh <- relayResult{
					Relay:  relayURL,
					Status: "error",
					Error:  syncErr.Error(),
				}
				return
			}

			if resp.Changed {
				log.Printf("relay %s: CHANGED (now %d users)", relayURL, resp.UserCount)
			}

			tracker.recordSuccess(relayURL, now, resp.UserCount, resp.Changed)
			metrics.recordRun(relayURL, "success", duration, 0)

			resultsCh <- relayResult{
				Relay:     relayURL,
				Status:    "ok",
				Changed:   resp.Changed,
				UserCount: resp.UserCount,
			}
		}(relayURL, token)
	}

	wg.Wait()
	close(resultsCh)

	results := make([]relayResult, 0, len(cfg.relayURLs))
	hasError := false
	for r := range resultsCh {
		if r.Status == "error" {
			hasError = true
		}
		results = append(results, r)
	}

	status := "ok"
	if hasError {
		status = "partial_error"
	}

	return &triggerResponse{
		Status:   status,
		UUIDs:    len(uuids),
		Relays:   results,
		Duration: time.Since(start).String(),
	}, nil
}

func mustLoadConfig() config {
	apiURL := os.Getenv("REMNAWAVE_API_URL")
	if apiURL == "" {
		log.Fatal("REMNAWAVE_API_URL is required")
	}

	apiToken := os.Getenv("REMNAWAVE_API_TOKEN")
	if apiToken == "" {
		log.Fatal("REMNAWAVE_API_TOKEN is required")
	}

	relayURLsStr := os.Getenv("RELAY_URLS")
	if relayURLsStr == "" {
		log.Fatal("RELAY_URLS is required")
	}

	relayTokensStr := os.Getenv("RELAY_TOKENS")
	if relayTokensStr == "" {
		log.Fatal("RELAY_TOKENS is required")
	}

	relayURLs := strings.Split(relayURLsStr, ",")
	relayTokens := strings.Split(relayTokensStr, ",")

	if len(relayURLs) != len(relayTokens) {
		log.Fatalf("RELAY_URLS count (%d) != RELAY_TOKENS count (%d)", len(relayURLs), len(relayTokens))
	}

	listenAddr := cmp.Or(os.Getenv("LISTEN_ADDR"), ":8080")

	syncInterval := 1 * time.Second
	if s := os.Getenv("SYNC_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			log.Fatalf("invalid SYNC_INTERVAL %q: %v", s, err)
		}
		syncInterval = d
	}

	return config{
		apiURL:       apiURL,
		apiToken:     apiToken,
		relayURLs:    relayURLs,
		relayTokens:  relayTokens,
		listenAddr:   listenAddr,
		syncInterval: syncInterval,
	}
}

func fetchActiveUUIDs(cfg config) ([]string, error) {
	url := strings.TrimRight(cfg.apiURL, "/") + "/api/users/active-vless-uuids"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.apiToken)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", "127.0.0.1")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result uuidsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return result.Response.UUIDs, nil
}

// relayClient is an HTTP client that skips TLS verification for relay agents
// using self-signed certificates.
var relayClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext:     (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	},
}

func pushToRelay(syncURL, token string, uuids []string) (*syncResponse, error) {
	payload, err := json.Marshal(syncRequest{UUIDs: uuids})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequest("POST", syncURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := relayClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result syncResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &result, nil
}
