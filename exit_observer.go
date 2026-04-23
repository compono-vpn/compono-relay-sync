package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Exit-node sync observer.
//
// Today relay-sync pushes the active-UUID allowlist to Sber relays and that's
// it — exit nodes (the remnawave-node containers on Netcup/Hetzner) get their
// user set via compono-backend's event-driven AddUserToNodeEvent pipeline,
// which has silent-failure modes we shipped BDT-27 logging for today. A
// manual audit (2026-04-23) found live drift on 2 of 4 exits.
//
// This observer runs every observeInterval and, for each connected exit
// node reported by the panel:
//
//  1. GET  /api/nodes/:uuid/expected-users   (new panel endpoint)
//  2. GET  /api/nodes/:uuid/actual-users     (new panel endpoint; proxies
//                                             get-inbound-users per active
//                                             inbound tag via the panel's
//                                             existing mTLS+JWT to the node)
//  3. Diff per inbound tag, emit Prometheus metrics
//
// Read-only. The BDT-27 event-driven handlers in compono-backend are still
// the writers — this observer just paints the drift picture so the new
// VMRule alerts (argocd-apps PR #6) can fire.
//
// Step 2 will flip this into a writer: once the observer is known-good and
// metrics are trusted, add add-user / remove-user reconciliation behind a
// per-exit feature flag + churn caps, then retire the backend events.

type panelNode struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	IsConnected bool   `json:"isConnected"`
	IsDisabled  bool   `json:"isDisabled"`
}

type expectedUsersResponse struct {
	Response struct {
		NodeUUID string `json:"nodeUuid"`
		Users    []struct {
			TID         int64    `json:"tId"`
			VlessUUID   string   `json:"vlessUuid"`
			Username    string   `json:"username"`
			InboundTags []string `json:"inboundTags"`
		} `json:"users"`
	} `json:"response"`
}

type actualUsersResponse struct {
	Response struct {
		NodeUUID string `json:"nodeUuid"`
		Users    []struct {
			Username    string   `json:"username"`
			InboundTags []string `json:"inboundTags"`
		} `json:"users"`
		UnreachableTags []string `json:"unreachableTags"`
	} `json:"response"`
}

type exitObserver struct {
	panelURL   string
	apiToken   string
	httpClient *http.Client
	metrics    *exitMetrics
	interval   time.Duration
}

func newExitObserver(panelURL, apiToken string, interval time.Duration, metrics *exitMetrics) *exitObserver {
	return &exitObserver{
		panelURL:   strings.TrimRight(panelURL, "/"),
		apiToken:   apiToken,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		metrics:    metrics,
		interval:   interval,
	}
}

func (o *exitObserver) run(ctx context.Context) {
	// Quick initial run so metrics appear before the first tick.
	if err := o.tick(ctx); err != nil {
		log.Printf("exit-observer initial tick: %v", err)
	}

	t := time.NewTicker(o.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := o.tick(ctx); err != nil {
				log.Printf("exit-observer tick: %v", err)
			}
		}
	}
}

func (o *exitObserver) tick(ctx context.Context) error {
	nodes, err := o.fetchConnectedNodes(ctx)
	if err != nil {
		o.metrics.observerErrors.WithLabelValues("fetch_nodes").Inc()
		return fmt.Errorf("fetch nodes: %w", err)
	}

	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n panelNode) {
			defer wg.Done()
			o.observeNode(ctx, n)
		}(n)
	}
	wg.Wait()
	return nil
}

func (o *exitObserver) observeNode(ctx context.Context, n panelNode) {
	expected, err := o.fetchExpectedUsers(ctx, n.UUID)
	if err != nil {
		log.Printf("exit-observer %s: fetch expected: %v", n.Name, err)
		o.metrics.observerErrors.WithLabelValues("fetch_expected").Inc()
		return
	}

	actual, err := o.fetchActualUsers(ctx, n.UUID)
	if err != nil {
		log.Printf("exit-observer %s: fetch actual: %v", n.Name, err)
		o.metrics.observerErrors.WithLabelValues("fetch_actual").Inc()
		return
	}

	// Index expected by (username → tag set) per user.
	// username in xray = t_id string in panel.
	type key struct {
		tag      string
		username string
	}
	expectedByTag := map[string]map[string]struct{}{}
	for _, u := range expected.Response.Users {
		username := fmt.Sprintf("%d", u.TID)
		for _, tag := range u.InboundTags {
			if expectedByTag[tag] == nil {
				expectedByTag[tag] = map[string]struct{}{}
			}
			expectedByTag[tag][username] = struct{}{}
		}
	}

	actualByTag := map[string]map[string]struct{}{}
	for _, u := range actual.Response.Users {
		for _, tag := range u.InboundTags {
			if actualByTag[tag] == nil {
				actualByTag[tag] = map[string]struct{}{}
			}
			actualByTag[tag][u.Username] = struct{}{}
		}
	}

	// Union of tags seen on either side — ensures we emit a zero for a
	// tag that disappeared, instead of silently dropping stale series.
	tags := map[string]struct{}{}
	for t := range expectedByTag {
		tags[t] = struct{}{}
	}
	for t := range actualByTag {
		tags[t] = struct{}{}
	}

	unreachable := map[string]struct{}{}
	for _, t := range actual.Response.UnreachableTags {
		unreachable[t] = struct{}{}
	}

	for tag := range tags {
		exp := expectedByTag[tag]
		act := actualByTag[tag]

		var missing, stale int
		if _, ur := unreachable[tag]; !ur {
			for u := range exp {
				if _, ok := act[u]; !ok {
					missing++
				}
			}
			for u := range act {
				if _, ok := exp[u]; !ok {
					stale++
				}
			}
		}

		o.metrics.expectedUsers.WithLabelValues(n.Name, tag).Set(float64(len(exp)))
		o.metrics.actualUsers.WithLabelValues(n.Name, tag).Set(float64(len(act)))
		o.metrics.missingUsers.WithLabelValues(n.Name, tag).Set(float64(missing))
		o.metrics.staleUsers.WithLabelValues(n.Name, tag).Set(float64(stale))
	}

	o.metrics.lastSuccess.WithLabelValues(n.Name).SetToCurrentTime()
}

func (o *exitObserver) fetchConnectedNodes(ctx context.Context) ([]panelNode, error) {
	var raw struct {
		Response []panelNode `json:"response"`
	}
	if err := o.getJSON(ctx, "/api/nodes", &raw); err != nil {
		return nil, err
	}
	out := make([]panelNode, 0, len(raw.Response))
	for _, n := range raw.Response {
		if n.IsConnected && !n.IsDisabled {
			out = append(out, n)
		}
	}
	return out, nil
}

func (o *exitObserver) fetchExpectedUsers(ctx context.Context, nodeUUID string) (*expectedUsersResponse, error) {
	var out expectedUsersResponse
	path := fmt.Sprintf("/api/nodes/%s/expected-users", nodeUUID)
	if err := o.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (o *exitObserver) fetchActualUsers(ctx context.Context, nodeUUID string) (*actualUsersResponse, error) {
	var out actualUsersResponse
	path := fmt.Sprintf("/api/nodes/%s/actual-users", nodeUUID)
	if err := o.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (o *exitObserver) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", o.panelURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+o.apiToken)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-For", "127.0.0.1")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
