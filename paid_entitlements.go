package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

var credentialUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func fetchPaidUUIDs(cfg config) ([]string, time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var snapshot struct {
		GeneratedAt  time.Time `json:"generated_at"`
		ValidUntil   time.Time `json:"valid_until"`
		Entitlements []struct {
			UserUUID  string    `json:"user_uuid"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"entitlements"`
	}
	if err := paidJSON(ctx, strings.TrimRight(cfg.billingURL, "/")+"/api/v1/internal/relay-entitlements", cfg.billingToken, false, &snapshot); err != nil {
		return nil, time.Time{}, err
	}
	now := time.Now()
	deadline := snapshot.ValidUntil
	if snapshot.GeneratedAt.IsZero() || snapshot.GeneratedAt.After(now.Add(5*time.Second)) || !deadline.After(now) || deadline.After(snapshot.GeneratedAt.Add(2*time.Minute)) || deadline.After(now.Add(2*time.Minute)) {
		return nil, time.Time{}, fmt.Errorf("invalid or expired billing entitlement snapshot")
	}
	ids := make([]string, 0, len(snapshot.Entitlements))
	for _, grant := range snapshot.Entitlements {
		if !credentialUUID.MatchString(grant.UserUUID) {
			return nil, time.Time{}, fmt.Errorf("invalid entitlement account ID")
		}
		if !grant.ExpiresAt.After(now) {
			continue
		}
		var user struct {
			Response struct {
				UUID      string    `json:"uuid"`
				VlessUUID string    `json:"vlessUuid"`
				Status    string    `json:"status"`
				ExpireAt  time.Time `json:"expireAt"`
			} `json:"response"`
		}
		if err := paidJSON(ctx, strings.TrimRight(cfg.apiURL, "/")+"/api/users/"+url.PathEscape(grant.UserUUID), cfg.apiToken, true, &user); err != nil {
			return nil, time.Time{}, err
		}
		u := user.Response
		if u.UUID != grant.UserUUID {
			return nil, time.Time{}, fmt.Errorf("account identity mismatch")
		}
		if u.Status != "ACTIVE" || !u.ExpireAt.After(now) {
			continue
		}
		if !credentialUUID.MatchString(u.VlessUUID) {
			return nil, time.Time{}, fmt.Errorf("invalid managed credential")
		}
		ids = append(ids, u.VlessUUID)
		if grant.ExpiresAt.Before(deadline) {
			deadline = grant.ExpiresAt
		}
		if u.ExpireAt.Before(deadline) {
			deadline = u.ExpireAt
		}
	}
	if !deadline.After(time.Now()) {
		return nil, time.Time{}, fmt.Errorf("entitlements expired during resolution")
	}
	slices.Sort(ids)
	return slices.Compact(ids), deadline, nil
}

func paidJSON(ctx context.Context, address, token string, panel bool, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	if panel {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
	} else {
		req.Header.Set("X-Internal-Secret", token)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("entitlement source request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("entitlement source status %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(out); err != nil {
		return fmt.Errorf("invalid entitlement source response: %w", err)
	}
	return nil
}

// Refuse legacy agents before submitting credentials they could retain forever.
func verifyPaidAgent(syncURL, token string) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(syncURL, "/sync")+"/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := relayClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var capability struct {
		Required bool `json:"authorization_required"`
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("paid agent health status %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&capability); err != nil {
		return err
	}
	if !capability.Required {
		return fmt.Errorf("relay does not enforce paid authorization leases")
	}
	return nil
}
