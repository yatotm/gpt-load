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
		"blacklist_window_minutes":        15,
		"consecutive_failure_threshold":   4,
	}
	keyConfig := datatypes.JSONMap{
		"active_probe_enabled":          false,
		"blacklist_threshold":           0,
		"consecutive_failure_threshold": 2,
	}

	effective := manager.GetEffectiveKeyConfig(groupConfig, keyConfig)
	if effective.ActiveProbeEnabled {
		t.Fatal("expected key override to disable active probe")
	}
	if effective.BlacklistThreshold != 0 {
		t.Fatalf("expected key override blacklist threshold to be 0, got %d", effective.BlacklistThreshold)
	}
	if effective.BlacklistWindowMinutes != 15 {
		t.Fatalf("expected group override failure window to be inherited, got %d", effective.BlacklistWindowMinutes)
	}
	if effective.ConsecutiveFailureThreshold != 2 {
		t.Fatalf("expected key override consecutive failure threshold to be 2, got %d", effective.ConsecutiveFailureThreshold)
	}
	if effective.ActiveProbeTimeoutSeconds != 45 {
		t.Fatalf("expected group override timeout to be inherited, got %d", effective.ActiveProbeTimeoutSeconds)
	}
	if effective.ActiveProbeFailureRateLimit != 12 {
		t.Fatalf("expected group override failure rate limit to be inherited, got %d", effective.ActiveProbeFailureRateLimit)
	}
}

func TestValidateGroupConfigOverridesAcceptsActiveProbeIdlePeriods(t *testing.T) {
	manager := NewSystemSettingsManager()

	if err := manager.ValidateGroupConfigOverrides(map[string]any{
		"active_probe_idle_periods": "00:00-08:00,23:00-06:00",
	}); err != nil {
		t.Fatalf("expected idle periods config to pass validation, got %v", err)
	}
}

func TestValidateGroupConfigOverridesRejectsInvalidActiveProbeIdlePeriods(t *testing.T) {
	manager := NewSystemSettingsManager()

	err := manager.ValidateGroupConfigOverrides(map[string]any{
		"active_probe_idle_periods": "08:00-08:00",
	})
	if err == nil {
		t.Fatal("expected invalid idle periods config to be rejected")
	}
}
