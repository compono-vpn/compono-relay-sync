# Paid relay synchronization (2026-10-07, not deployed)

Opt-in PAID_RELAY_ENABLED=true requires BILLING_API_URL,
BILLING_INTERNAL_SECRET and SYNC_INTERVAL between zero (exclusive) and 30s.
Deploy first as an isolated canary targeting only a compatible paid-mode agent.
Legacy/default sync remains unchanged; do not enable against the live fleet yet.

The controller fetches billing's authenticated relay-entitlements snapshot,
rejects expired/overlong snapshots, resolves each managed account from the native
VPN API and includes only active, unexpired accounts. It sends VLESS credentials
with a deadline no later than the billing snapshot, subscription expiry or VPN
account expiry. A whole resolution has a 20s deadline. Source failures do not
renew a lease; the agent must clear it locally when it expires. Unchanged paid
membership still renews the lease every cycle (bypassing the legacy hash skip).

Before posting, the target health must advertise authorization_required=true.
The sync reply must acknowledge the exact deadline. This prevents silently
using a legacy agent that ignores leases. Neither host visibility nor squads
establish paid access. Billing remains authoritative for non-trial entitlements.

Tests cover expiry bounding, stale snapshots, source authentication, rejection
of legacy agents before credentials are posted, and end-to-end lease JSON
acknowledgement. Full race suite passes. Remaining: crash-safe agent deployment,
live canary admission/revocation tests, subscription visibility and rollout.

Live isolated integration now verified: production billing v0.40.0 -> native
account lookup -> this controller -> loopback-only paid agent, 26 credentials,
lease renewal acknowledged and no controller failures. Production enforcement
is still disabled. Synthetic traffic admission/expiry/watchdog evidence is in
compono-infra/docs/repro/2026-10-07-paid-relay; real-account traffic remains a gate.

Configuration parsing rejects invalid PAID_RELAY_ENABLED values instead of
silently selecting legacy mode. Empty/false retain legacy behavior; true enables
paid mode (case and surrounding whitespace normalized). Test-first regression
and complete race suite passed on 2026-10-07.

Real-account traffic now passed against the selective-revocation agent/Xray
canary: current production billing/native reads populated 26 paid grants; one
paid account transferred 128,000 bytes over 125.15 seconds and an active trial
account was rejected before/after. No billing records changed. The controller
was stopped and canary grants explicitly cleared and read back. This verifies
the control-plane decision plus plain TCP runtime authorization on the isolated
host, not production transport or mobile coverage. Production remains unchanged.
