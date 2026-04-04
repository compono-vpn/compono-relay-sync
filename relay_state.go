package main

import (
	"math"
	"sync"
	"time"
)

const (
	// backoffBase is the initial backoff delay after the first failure.
	backoffBase = 2 * time.Second
	// backoffMax is the maximum backoff delay.
	backoffMax = 60 * time.Second
	// degradedThreshold is the number of consecutive failures before a relay
	// is considered degraded in the health check.
	degradedThreshold = 3
)

// relayState tracks per-relay sync state.
type relayState struct {
	LastAttempt   time.Time `json:"last_attempt"`
	LastSuccess   time.Time `json:"last_success"`
	LastError     string    `json:"last_error"`
	ConsecFails   int       `json:"consecutive_failures"`
	LastUserCount int       `json:"last_user_count"`
	LastChanged   bool      `json:"last_changed"`
}

// backoffDelay returns the backoff duration for the current failure count.
// Returns 0 if there are no failures (no backoff).
func (s relayState) backoffDelay() time.Duration {
	if s.ConsecFails == 0 {
		return 0
	}
	delay := float64(backoffBase) * math.Pow(2, float64(s.ConsecFails-1))
	if delay > float64(backoffMax) {
		delay = float64(backoffMax)
	}
	return time.Duration(delay)
}

// shouldSkip returns true if the relay is in backoff and the next attempt
// should be skipped. The caller can bypass this for manual triggers.
func (s relayState) shouldSkip(now time.Time) bool {
	d := s.backoffDelay()
	if d == 0 {
		return false
	}
	return now.Before(s.LastAttempt.Add(d))
}

// relayTracker holds state for all relays. Safe for concurrent access.
type relayTracker struct {
	mu     sync.RWMutex
	states map[string]*relayState
}

func newRelayTracker(relayURLs []string) *relayTracker {
	t := &relayTracker{
		states: make(map[string]*relayState, len(relayURLs)),
	}
	for _, u := range relayURLs {
		t.states[u] = &relayState{}
	}
	return t
}

// recordSuccess records a successful sync for the given relay.
func (t *relayTracker) recordSuccess(relay string, now time.Time, userCount int, changed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.states[relay]
	if s == nil {
		return
	}
	s.LastAttempt = now
	s.LastSuccess = now
	s.LastError = ""
	s.ConsecFails = 0
	s.LastUserCount = userCount
	s.LastChanged = changed
}

// recordFailure records a failed sync for the given relay.
func (t *relayTracker) recordFailure(relay string, now time.Time, errMsg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.states[relay]
	if s == nil {
		return
	}
	s.LastAttempt = now
	s.LastError = errMsg
	s.ConsecFails++
}

// shouldSkip checks if a relay should be skipped due to backoff.
func (t *relayTracker) shouldSkip(relay string, now time.Time) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s := t.states[relay]
	if s == nil {
		return false
	}
	return s.shouldSkip(now)
}

// consecutiveFailures returns the current consecutive failure count for a relay.
func (t *relayTracker) consecutiveFailures(relay string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s := t.states[relay]
	if s == nil {
		return 0
	}
	return s.ConsecFails
}

// snapshot returns a copy of all relay states for status reporting.
func (t *relayTracker) snapshot() map[string]relayState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]relayState, len(t.states))
	for k, v := range t.states {
		out[k] = *v
	}
	return out
}
