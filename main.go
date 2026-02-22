package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type config struct {
	apiURL     string
	apiToken   string
	relayURLs  []string
	relayTokens []string
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

func main() {
	cfg := mustLoadConfig()

	log.Printf("relay-sync: api=%s relays=%d", cfg.apiURL, len(cfg.relayURLs))

	uuids, err := fetchActiveUUIDs(cfg)
	if err != nil {
		log.Fatalf("failed to fetch active UUIDs: %v", err)
	}

	log.Printf("fetched %d active UUIDs", len(uuids))

	hasError := false
	for i, relayURL := range cfg.relayURLs {
		token := cfg.relayTokens[i]
		syncURL := strings.TrimRight(relayURL, "/") + "/sync"

		resp, err := pushToRelay(syncURL, token, uuids)
		if err != nil {
			log.Printf("relay %s: ERROR: %v", relayURL, err)
			hasError = true
			continue
		}

		if resp.Changed {
			log.Printf("relay %s: CHANGED (now %d users)", relayURL, resp.UserCount)
		} else {
			log.Printf("relay %s: unchanged (%d users)", relayURL, resp.UserCount)
		}
	}

	if hasError {
		os.Exit(1)
	}
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

	return config{
		apiURL:      apiURL,
		apiToken:    apiToken,
		relayURLs:   relayURLs,
		relayTokens: relayTokens,
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
	defer resp.Body.Close()

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

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

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
