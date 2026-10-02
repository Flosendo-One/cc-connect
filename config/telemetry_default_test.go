package config

import "testing"

func boolp(b bool) *bool { return &b }

// Flosendo fork (EDEN-1313, 2 Oct 2026): telemetry is OFF unless a deployment opts in explicitly
// AND names its own PostHog project key. The upstream default (on, with a built-in third-party
// key, raw message text) sent every Slack message to a project we do not own.
func TestTelemetryIsOffByDefault(t *testing.T) {
	if (TelemetryConfig{}).TelemetryEnabled() {
		t.Fatal("an empty [telemetry] section (or none) must mean telemetry is off")
	}
}

func TestTelemetryOptInNeedsItsOwnKey(t *testing.T) {
	if (TelemetryConfig{Disabled: boolp(false)}).TelemetryEnabled() {
		t.Fatal("opting in without api_key must stay off: there is no built-in key any more")
	}
	if (TelemetryConfig{Enabled: boolp(true)}).TelemetryEnabled() {
		t.Fatal("legacy enabled=true without api_key must stay off")
	}
	if !(TelemetryConfig{Disabled: boolp(false), APIKey: "phc_ours"}).TelemetryEnabled() {
		t.Fatal("explicit opt-in with our own key must be on")
	}
	if (TelemetryConfig{Disabled: boolp(true), APIKey: "phc_ours"}).TelemetryEnabled() {
		t.Fatal("disabled=true always wins")
	}
}

func TestNoBuiltInTelemetryKey(t *testing.T) {
	if got := (TelemetryConfig{}).EffectiveAPIKey(); got != "" {
		t.Fatalf("EffectiveAPIKey must not fall back to a built-in key, got %q", got)
	}
}
