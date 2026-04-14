package keypool

import (
	"fmt"
	"gpt-load/internal/config"
	"gpt-load/internal/encryption"
	"gpt-load/internal/models"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gpt-load/internal/store"
	"gpt-load/internal/types"
)

func TestRecordProbeWindowRequiresCompleteInitialWindow(t *testing.T) {
	checker := &CronChecker{Store: store.NewMemoryStore()}
	start := time.Unix(1_700_000_000, 0)

	stats, err := checker.recordProbeWindow(1, 10, false, start)
	if err != nil {
		t.Fatalf("recordProbeWindow returned error: %v", err)
	}
	if stats.WindowComplete {
		t.Fatal("expected first sample to stay in warmup window")
	}

	stats, err = checker.recordProbeWindow(1, 10, false, start.Add(9*time.Minute))
	if err != nil {
		t.Fatalf("recordProbeWindow returned error: %v", err)
	}
	if stats.WindowComplete {
		t.Fatal("expected warmup window to remain incomplete before 10 minutes")
	}

	stats, err = checker.recordProbeWindow(1, 10, false, start.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("recordProbeWindow returned error: %v", err)
	}
	if !stats.WindowComplete {
		t.Fatal("expected window to become complete after the full first window")
	}
}

func TestDecideProbeStatusChangeSkipsWarmupWindow(t *testing.T) {
	decision := decideProbeStatusChange(probeWindowStats{
		SampleCount:    3,
		FailureRate:    100,
		WindowComplete: false,
	}, true, 10)

	if decision.ShouldBlacklist || decision.ShouldRestore {
		t.Fatal("expected warmup window to skip active blacklist/restore decisions")
	}
}

func TestHasKeySpecificActiveProbeConfig(t *testing.T) {
	if !hasKeySpecificActiveProbeConfig(datatypes.JSONMap{"active_probe_enabled": true}) {
		t.Fatal("expected active probe override to be detected")
	}
	if hasKeySpecificActiveProbeConfig(datatypes.JSONMap{"blacklist_threshold": 0}) {
		t.Fatal("expected unrelated key override not to count as active probe override")
	}
}

func TestShouldUseActiveProbeForKey(t *testing.T) {
	activeConfig := types.SystemSettings{ActiveProbeEnabled: true}
	inactiveConfig := types.SystemSettings{ActiveProbeEnabled: false}

	if shouldUseActiveProbeForKey(false, nil, inactiveConfig) {
		t.Fatal("expected inactive probe config to disable active probing")
	}
	if !shouldUseActiveProbeForKey(false, nil, activeConfig) {
		t.Fatal("expected active probing to run outside idle periods")
	}
	if shouldUseActiveProbeForKey(true, nil, activeConfig) {
		t.Fatal("expected idle periods to disable group-default active probing")
	}
	if !shouldUseActiveProbeForKey(true, datatypes.JSONMap{"active_probe_enabled": true}, activeConfig) {
		t.Fatal("expected key-level active probe override to bypass idle-period disable")
	}
}

func TestDecideProbeStatusForDisplayRequiresCompleteWindow(t *testing.T) {
	decision := DecideProbeStatusForDisplay(probeWindowStats{
		SampleCount:    4,
		FailureRate:    100,
		WindowComplete: false,
	}, 10)

	if decision.ShouldBlacklist || decision.ShouldRestore {
		t.Fatal("expected incomplete window to stay neutral for display")
	}
}

func TestProbeGroupKeysSkipsDisabledKeys(t *testing.T) {
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}
	if err := db.AutoMigrate(&models.APIKey{}); err != nil {
		t.Fatalf("failed to migrate api_keys: %v", err)
	}

	encryptionSvc, err := encryption.NewService("")
	if err != nil {
		t.Fatalf("failed to init encryption service: %v", err)
	}

	settingsManager := config.NewSystemSettingsManager()
	memStore := store.NewMemoryStore()
	provider := NewProvider(db, memStore, settingsManager, encryptionSvc)
	checker := &CronChecker{
		DB:              db,
		SettingsManager: settingsManager,
		EncryptionSvc:   encryptionSvc,
		KeyProvider:     provider,
		Store:           memStore,
		stopChan:        make(chan struct{}),
	}

	key := models.APIKey{
		GroupID:   31,
		KeyValue:  "disabled-key",
		KeyHash:   "hash-disabled-key",
		Status:    models.KeyStatusDisabled,
		Priority:  models.DefaultAPIKeyPriority,
		CreatedAt: time.Now(),
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("failed to create key: %v", err)
	}

	group := &models.Group{
		ID: 31,
		Config: datatypes.JSONMap{
			"active_probe_enabled":            true,
			"active_probe_interval_seconds":   1,
			"active_probe_timeout_seconds":    1,
			"active_probe_window_minutes":     10,
			"active_probe_failure_rate_limit": 10,
		},
	}
	group.EffectiveConfig = settingsManager.GetEffectiveConfig(group.Config)

	checker.probeGroupKeys(group)

	var updated models.APIKey
	if err := db.First(&updated, key.ID).Error; err != nil {
		t.Fatalf("failed to reload key: %v", err)
	}
	if updated.LastProbeAt != nil {
		t.Fatalf("expected disabled key to skip probing, got last_probe_at=%v", updated.LastProbeAt)
	}
}
