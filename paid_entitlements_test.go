package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPaidEntitlementsIntersectAccountAndExpiry(t *testing.T) {
	now := time.Now().UTC()
	account := "11111111-1111-1111-1111-111111111111"
	credential := "22222222-2222-2222-2222-222222222222"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/internal/relay-entitlements" {
			if r.Header.Get("X-Internal-Secret") != "billing-secret" {
				t.Error("missing billing authentication")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"generated_at": now, "valid_until": now.Add(time.Minute), "entitlements": []any{map[string]any{"user_uuid": account, "expires_at": now.Add(30 * time.Second)}}})
			return
		}
		if r.Header.Get("Authorization") != "Bearer panel-secret" {
			t.Error("missing panel authentication")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"uuid": account, "vlessUuid": credential, "status": "ACTIVE", "expireAt": now.Add(time.Hour)}})
	}))
	defer server.Close()
	ids, deadline, err := fetchPaidUUIDs(config{billingURL: server.URL, billingToken: "billing-secret", apiURL: server.URL, apiToken: "panel-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != credential {
		t.Fatal("did not resolve paid account credential")
	}
	if !deadline.Equal(now.Add(30 * time.Second)) {
		t.Fatal("lease outlived subscription")
	}
}

func TestPaidEntitlementsRejectStaleSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"generated_at": time.Now().Add(-time.Hour), "valid_until": time.Now().Add(-time.Minute), "entitlements": []any{}})
	}))
	defer server.Close()
	if _, _, err := fetchPaidUUIDs(config{billingURL: server.URL}); err == nil {
		t.Fatal("accepted expired snapshot")
	}
}

func TestPaidPushRejectsLegacyAgentBeforeSendingCredentials(t *testing.T) {
	posted := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	}))
	defer server.Close()
	if _, err := pushToRelay(server.URL+"/sync", "secret", []string{"test"}, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("accepted legacy agent")
	}
	if posted {
		t.Fatal("sent credentials before capability verification")
	}
}

func TestPaidPushAcknowledgesExactLease(t *testing.T) {
	deadline := time.Now().UTC().Add(time.Minute)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{"authorization_required": true})
			return
		}
		var request syncRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.ValidUntil == nil || !request.ValidUntil.Equal(deadline) {
			t.Error("deadline not transmitted")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "authorization_valid_until": request.ValidUntil})
	}))
	defer server.Close()
	if _, err := pushToRelay(server.URL+"/sync", "secret", []string{"test"}, deadline); err != nil {
		t.Fatal(err)
	}
}
