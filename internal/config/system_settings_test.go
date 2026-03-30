package config

import (
	"testing"

	"gorm.io/datatypes"
)

func TestGetEffectiveKeyConfig(t *testing.T) {
	manager := NewSystemSettingsManager()

	groupConfig := datatypes.JSONMap{
		"active_probe_enabled":            true,
		"active_probe_timeout_seconds":    45,
		"active_probe_failure_rate_limit": 12,
		"blacklist_threshold":             3,
	}
	keyConfig := datatypes.JSONMap{
		"active_probe_enabled": false,
		"blacklist_threshold":  0,
	}

	effective := manager.GetEffectiveKeyConfig(groupConfig, keyConfig)
	if effective.ActiveProbeEnabled {
		t.Fatal("expected key override to disable active probe")
	}
	if effective.BlacklistThreshold != 0 {
		t.Fatalf("expected key override blacklist threshold to be 0, got %d", effective.BlacklistThreshold)
	}
	if effective.ActiveProbeTimeoutSeconds != 45 {
		t.Fatalf("expected group override timeout to be inherited, got %d", effective.ActiveProbeTimeoutSeconds)
	}
	if effective.ActiveProbeFailureRateLimit != 12 {
		t.Fatalf("expected group override failure rate limit to be inherited, got %d", effective.ActiveProbeFailureRateLimit)
	}
}
