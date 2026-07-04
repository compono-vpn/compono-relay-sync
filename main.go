package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
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
	apiURL                string
	apiToken              string
	relays                []relayTarget
	listenAddr            string
	syncInterval          time.Duration
	activeUUIDState       *activeUUIDState
	exitObserverInterval  time.Duration
	exitObserverReconcile bool
}

type relayTarget struct {
	Name  string
	URL   string
	Token string
}

type relayConfig struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Token string `json:"token"`
}

type uuidsResponse struct {
	Response struct {
		UUIDs []string `json:"uuids"`
	} `json:"response"`
}

type activeUUIDState struct {
	mu      sync.RWMutex
	etag    string
	hash    string
	uuids   []string
	hasData bool
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
	tracker := newRelayTracker(relayNames(cfg.relays))
	metrics := newSyncMetrics()
	exitMetrics := newExitMetrics()

	log.Printf("relay-sync: api=%s relays=%d listen=%s interval=%s exit_observer_interval=%s exit_observer_reconcile=%t",
		cfg.apiURL, len(cfg.relays), cfg.listenAddr, cfg.syncInterval, cfg.exitObserverInterval, cfg.exitObserverReconcile)

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
			RelayCount:   len(cfg.relays),
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

	// Exit-node drift observer (step-1: metrics only, no reconciliation).
	// Skipped entirely if EXIT_OBSERVER_INTERVAL is 0 — avoids perma-spam
	// during local dev where the panel isn't reachable.
	if cfg.exitObserverInterval > 0 {
		observer := newExitObserver(
			cfg.apiURL,
			cfg.apiToken,
			cfg.exitObserverInterval,
			exitMetrics,
			cfg.exitObserverReconcile,
		)
		go observer.run(ctx)
	} else {
		log.Println("exit-observer disabled (EXIT_OBSERVER_INTERVAL=0)")
	}

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
	if cfg.activeUUIDState == nil {
		cfg.activeUUIDState = &activeUUIDState{}
	}

	uuids, changed, err := fetchActiveUUIDs(cfg)
	if err != nil {
		return nil, fmt.Errorf("fetch UUIDs: %w", err)
	}

	if !changed {
		results := make([]relayResult, 0, len(cfg.relays))
		for _, relay := range cfg.relays {
			results = append(results, relayResult{Relay: relay.Name, Status: "skipped", Skipped: true})
			metrics.recordRun(relay.Name, "skipped", 0, 0)
		}

		return &triggerResponse{
			Status:   "ok",
			UUIDs:    len(uuids),
			Relays:   results,
			Duration: time.Since(start).String(),
		}, nil
	}

	resultsCh := make(chan relayResult, len(cfg.relays))
	var wg sync.WaitGroup

	for _, relay := range cfg.relays {
		target := relay

		// Check backoff unless this is a manual trigger.
		if !bypassBackoff && tracker.shouldSkip(target.Name, time.Now()) {
			log.Printf("relay %s (%s): skipped (backoff, %d consecutive failures)",
				target.Name, target.URL, tracker.consecutiveFailures(target.Name))
			resultsCh <- relayResult{
				Relay:   target.Name,
				Status:  "skipped",
				Skipped: true,
			}
			continue
		}

		wg.Add(1)
		go func(relay relayTarget) {
			defer wg.Done()
			syncURL := strings.TrimRight(relay.URL, "/") + "/sync"
			relayStart := time.Now()

			resp, syncErr := pushToRelay(syncURL, relay.Token, uuids)
			duration := time.Since(relayStart).Seconds()
			now := time.Now()

			if syncErr != nil {
				log.Printf("relay %s (%s): ERROR: %v", relay.Name, relay.URL, syncErr)
				tracker.recordFailure(relay.Name, now, syncErr.Error())
				consecFails := tracker.consecutiveFailures(relay.Name)
				metrics.recordRun(relay.Name, "error", duration, consecFails)
				resultsCh <- relayResult{
					Relay:  relay.Name,
					Status: "error",
					Error:  syncErr.Error(),
				}
				return
			}

			if resp.Changed {
				log.Printf("relay %s (%s): CHANGED (now %d users)", relay.Name, relay.URL, resp.UserCount)
			}

			tracker.recordSuccess(relay.Name, now, resp.UserCount, resp.Changed)
			metrics.recordRun(relay.Name, "success", duration, 0)

			resultsCh <- relayResult{
				Relay:     relay.Name,
				Status:    "ok",
				Changed:   resp.Changed,
				UserCount: resp.UserCount,
			}
		}(target)
	}

	wg.Wait()
	close(resultsCh)

	results := make([]relayResult, 0, len(cfg.relays))
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

	relays, err := parseRelayConfig(
		os.Getenv("RELAY_CONFIG"),
		os.Getenv("RELAY_URLS"),
		os.Getenv("RELAY_TOKENS"),
	)
	if err != nil {
		log.Fatal(err)
	}

	listenAddr := cmp.Or(os.Getenv("LISTEN_ADDR"), ":8080")

	syncInterval := 10 * time.Second
	if s := os.Getenv("SYNC_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			log.Fatalf("invalid SYNC_INTERVAL %q: %v", s, err)
		}
		syncInterval = d
	}
	activeUUIDState := &activeUUIDState{}

	exitObserverInterval := 60 * time.Second
	if s := os.Getenv("EXIT_OBSERVER_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			log.Fatalf("invalid EXIT_OBSERVER_INTERVAL %q: %v", s, err)
		}
		exitObserverInterval = d
	}

	// EXIT_OBSERVER_RECONCILE: when "true", every observer cycle calls
	// POST /api/nodes/:uuid/reconcile-users on compono-backend AFTER the
	// drift metrics are emitted. The backend then issues add-user /
	// remove-user RPCs to xray. Default off so a fresh deploy is safe.
	exitObserverReconcile := false
	if s := strings.ToLower(strings.TrimSpace(os.Getenv("EXIT_OBSERVER_RECONCILE"))); s != "" {
		switch s {
		case "1", "true", "yes", "on":
			exitObserverReconcile = true
		case "0", "false", "no", "off":
			exitObserverReconcile = false
		default:
			log.Fatalf("invalid EXIT_OBSERVER_RECONCILE %q (want true/false)", s)
		}
	}

	return config{
		apiURL:                apiURL,
		apiToken:              apiToken,
		relays:                relays,
		listenAddr:            listenAddr,
		syncInterval:          syncInterval,
		activeUUIDState:       activeUUIDState,
		exitObserverInterval:  exitObserverInterval,
		exitObserverReconcile: exitObserverReconcile,
	}
}

func parseRelayConfig(relayConfigJSON, relayURLsStr, relayTokensStr string) ([]relayTarget, error) {
	if configJSON := strings.TrimSpace(relayConfigJSON); configJSON != "" {
		var entries []relayConfig
		if err := json.Unmarshal([]byte(configJSON), &entries); err != nil {
			return nil, fmt.Errorf("invalid RELAY_CONFIG JSON: %w", err)
		}
		return validateRelayConfig(entries)
	}

	legacyURLs, err := parseCSVList("RELAY_URLS", relayURLsStr)
	if err != nil {
		return nil, err
	}

	legacyTokens, err := parseCSVList("RELAY_TOKENS", relayTokensStr)
	if err != nil {
		return nil, err
	}

	if len(legacyURLs) != len(legacyTokens) {
		return nil, fmt.Errorf("RELAY_URLS count (%d) != RELAY_TOKENS count (%d)", len(legacyURLs), len(legacyTokens))
	}

	entries := make([]relayConfig, len(legacyURLs))
	for i, u := range legacyURLs {
		entries[i] = relayConfig{
			Name:  legacyRelayName(u, i),
			URL:   u,
			Token: legacyTokens[i],
		}
	}

	return validateRelayConfig(entries)
}

func parseCSVList(name, value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	raw := strings.Split(value, ",")
	out := make([]string, 0, len(raw))
	for i, v := range raw {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, fmt.Errorf("%s has blank element at index %d", name, i)
		}
		out = append(out, v)
	}
	return out, nil
}

func validateRelayConfig(entries []relayConfig) ([]relayTarget, error) {
	seen := make(map[string]struct{}, len(entries))
	out := make([]relayTarget, 0, len(entries))
	for i, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			return nil, fmt.Errorf("RELAY_CONFIG entry %d: missing name", i)
		}
		u := strings.TrimSpace(entry.URL)
		if u == "" {
			return nil, fmt.Errorf("RELAY_CONFIG entry %d (%s): missing url", i, name)
		}
		if _, err := url.ParseRequestURI(u); err != nil {
			return nil, fmt.Errorf("RELAY_CONFIG entry %d (%s): invalid url %q: %v", i, name, u, err)
		}
		token := strings.TrimSpace(entry.Token)
		if token == "" {
			return nil, fmt.Errorf("RELAY_CONFIG entry %d (%s): missing token", i, name)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("RELAY_CONFIG has duplicate relay name %q", name)
		}
		seen[name] = struct{}{}
		out = append(out, relayTarget{
			Name:  name,
			URL:   u,
			Token: token,
		})
	}
	return out, nil
}

func relayNames(relays []relayTarget) []string {
	names := make([]string, 0, len(relays))
	for _, r := range relays {
		names = append(names, r.Name)
	}
	return names
}

func legacyRelayName(rawURL string, index int) string {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || parsed.Host == "" {
		return fmt.Sprintf("relay-%d", index+1)
	}
	return parsed.Host
}

func fetchActiveUUIDs(cfg config) ([]string, bool, error) {
	if cfg.activeUUIDState == nil {
		cfg.activeUUIDState = &activeUUIDState{}
	}

	url := strings.TrimRight(cfg.apiURL, "/") + "/api/users/active-vless-uuids"
	etag := cfg.activeUUIDState.etagHeader()

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("create request: %w", err)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.apiToken)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", "127.0.0.1")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotModified {
		return cfg.activeUUIDState.snapshotUUIDs(), false, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var result uuidsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, false, fmt.Errorf("parse response: %w", err)
	}

	uuids := result.Response.UUIDs
	if uuids == nil {
		uuids = []string{}
	}
	slices.Sort(uuids)
	h := hashUUIDs(uuids)
	respTag := strings.Trim(resp.Header.Get("ETag"), "\"")

	current := cfg.activeUUIDState.snapshotHash()
	changed := true
	if current.hasData {
		changed = h != current.hash
	}

	cfg.activeUUIDState.replace(uuids, h, respTag)

	return uuids, changed, nil
}

func hashUUIDs(uuids []string) string {
	if len(uuids) == 0 {
		return "empty"
	}
	sum := sha256.Sum256([]byte(strings.Join(uuids, ",")))
	return hex.EncodeToString(sum[:])
}

type activeUUIDStateSnapshot struct {
	hash    string
	etag    string
	uuids   []string
	hasData bool
}

func (s *activeUUIDState) etagHeader() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.etag
}

func (s *activeUUIDState) replace(uuids []string, hash, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hash = hash
	s.etag = etag
	s.hasData = true
	s.uuids = append(make([]string, 0, len(uuids)), uuids...)
}

func (s *activeUUIDState) snapshotHash() activeUUIDStateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return activeUUIDStateSnapshot{
		hash:    s.hash,
		etag:    s.etag,
		uuids:   append([]string(nil), s.uuids...),
		hasData: s.hasData,
	}
}

func (s *activeUUIDState) snapshotUUIDs() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.hasData {
		return nil
	}
	return append([]string(nil), s.uuids...)
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
