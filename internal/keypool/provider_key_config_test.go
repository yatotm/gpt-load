package keypool

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/config"
	"gpt-load/internal/encryption"
	"gpt-load/internal/models"
	"gpt-load/internal/store"

	"github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func newTestKeyProviderWithSettings(t *testing.T) (*KeyProvider, *gorm.DB, store.Store) {
	t.Helper()

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
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

	memStore := store.NewMemoryStore()
	provider := NewProvider(db, memStore, config.NewSystemSettingsManager(), encryptionSvc)
	return provider, db, memStore
}

func seedFailureWindow(t *testing.T, dataStore store.Store, keyID uint, entries []failureWindowEntry) {
	t.Helper()

	payload, err := json.Marshal(failureWindowState{Entries: entries})
	if err != nil {
		t.Fatalf("failed to marshal failure window: %v", err)
	}
	if err := dataStore.Set(failureWindowStoreKey(keyID), payload, time.Minute); err != nil {
		t.Fatalf("failed to seed failure window: %v", err)
	}
}

func TestHandleFailureUsesKeySpecificBlacklistThreshold(t *testing.T) {
	provider, db, dataStore := newTestKeyProviderWithSettings(t)

	key := models.APIKey{
		GroupID:   7,
		KeyValue:  "threshold-key",
		KeyHash:   "hash-threshold",
		Status:    models.KeyStatusActive,
		Priority:  models.DefaultAPIKeyPriority,
		Config:    datatypes.JSONMap{"blacklist_threshold": 0},
		CreatedAt: time.Now(),
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("failed to create key: %v", err)
	}

	keyHashKey := fmt.Sprintf("key:%d", key.ID)
	if err := dataStore.HSet(keyHashKey, map[string]any{
		"status":        models.KeyStatusActive,
		"failure_count": 0,
		"priority":      models.DefaultAPIKeyPriority,
	}); err != nil {
		t.Fatalf("failed to seed key hash: %v", err)
	}

	group := &models.Group{
		ID:     key.GroupID,
		Config: datatypes.JSONMap{"blacklist_threshold": 1},
	}
	group.EffectiveConfig = provider.settingsManager.GetEffectiveConfig(group.Config)

	if err := provider.handleFailure(&models.APIKey{ID: key.ID}, group, keyHashKey, 1); err != nil {
		t.Fatalf("handleFailure returned error: %v", err)
	}

	var updated models.APIKey
	if err := db.First(&updated, key.ID).Error; err != nil {
		t.Fatalf("failed to reload key: %v", err)
	}
	if updated.Status != models.KeyStatusActive {
		t.Fatalf("expected key to remain active, got %s", updated.Status)
	}
	if updated.FailureCount != 1 {
		t.Fatalf("expected failure count to increase to 1, got %v", updated.FailureCount)
	}
}

func TestHandleFailureUsesWeightedPenalty(t *testing.T) {
	provider, db, dataStore := newTestKeyProviderWithSettings(t)

	key := models.APIKey{
		GroupID:      9,
		KeyValue:     "weighted-key",
		KeyHash:      "hash-weighted",
		Status:       models.KeyStatusActive,
		Priority:     models.DefaultAPIKeyPriority,
		FailureCount: 0,
		CreatedAt:    time.Now(),
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("failed to create key: %v", err)
	}

	keyHashKey := fmt.Sprintf("key:%d", key.ID)
	if err := dataStore.HSet(keyHashKey, map[string]any{
		"status":        models.KeyStatusActive,
		"failure_count": 0,
		"priority":      models.DefaultAPIKeyPriority,
	}); err != nil {
		t.Fatalf("failed to seed key hash: %v", err)
	}

	group := &models.Group{
		ID:     key.GroupID,
		Config: datatypes.JSONMap{"blacklist_threshold": 1},
	}
	group.EffectiveConfig = provider.settingsManager.GetEffectiveConfig(group.Config)

	if err := provider.handleFailure(&models.APIKey{ID: key.ID}, group, keyHashKey, 0.5); err != nil {
		t.Fatalf("handleFailure returned error: %v", err)
	}

	var updated models.APIKey
	if err := db.First(&updated, key.ID).Error; err != nil {
		t.Fatalf("failed to reload key: %v", err)
	}
	if updated.Status != models.KeyStatusActive {
		t.Fatalf("expected key to remain active, got %s", updated.Status)
	}
	if updated.FailureCount != 0.5 {
		t.Fatalf("expected failure count to increase to 0.5, got %v", updated.FailureCount)
	}
}

func TestHandleFailureUsesRollingWindowCount(t *testing.T) {
	provider, db, dataStore := newTestKeyProviderWithSettings(t)

	key := models.APIKey{
		GroupID:   11,
		KeyValue:  "window-key",
		KeyHash:   "hash-window",
		Status:    models.KeyStatusActive,
		Priority:  models.DefaultAPIKeyPriority,
		CreatedAt: time.Now(),
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("failed to create key: %v", err)
	}

	keyHashKey := fmt.Sprintf("key:%d", key.ID)
	if err := dataStore.HSet(keyHashKey, map[string]any{
		"status":                    models.KeyStatusActive,
		"failure_count":             0,
		"consecutive_failure_count": 0,
		"priority":                  models.DefaultAPIKeyPriority,
	}); err != nil {
		t.Fatalf("failed to seed key hash: %v", err)
	}
	seedFailureWindow(t, dataStore, key.ID, []failureWindowEntry{
		{Timestamp: time.Now().Add(-2 * time.Minute).Unix(), Penalty: 1},
	})

	group := &models.Group{
		ID: key.GroupID,
		Config: datatypes.JSONMap{
			"blacklist_threshold":      2,
			"blacklist_window_minutes": 1,
		},
	}
	group.EffectiveConfig = provider.settingsManager.GetEffectiveConfig(group.Config)

	if err := provider.handleFailure(&models.APIKey{ID: key.ID}, group, keyHashKey, 1); err != nil {
		t.Fatalf("handleFailure returned error: %v", err)
	}

	var updated models.APIKey
	if err := db.First(&updated, key.ID).Error; err != nil {
		t.Fatalf("failed to reload key: %v", err)
	}
	if updated.Status != models.KeyStatusActive {
		t.Fatalf("expected key to remain active, got %s", updated.Status)
	}
	if updated.FailureCount != 1 {
		t.Fatalf("expected failure count to be 1 after expired entries were pruned, got %v", updated.FailureCount)
	}
}

func TestHandleFailureUsesConsecutiveThreshold(t *testing.T) {
	provider, db, dataStore := newTestKeyProviderWithSettings(t)

	key := models.APIKey{
		GroupID:   12,
		KeyValue:  "consecutive-key",
		KeyHash:   "hash-consecutive",
		Status:    models.KeyStatusActive,
		Priority:  models.DefaultAPIKeyPriority,
		CreatedAt: time.Now(),
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("failed to create key: %v", err)
	}

	keyHashKey := fmt.Sprintf("key:%d", key.ID)
	if err := dataStore.HSet(keyHashKey, map[string]any{
		"status":                    models.KeyStatusActive,
		"failure_count":             0,
		"consecutive_failure_count": 1,
		"priority":                  models.DefaultAPIKeyPriority,
	}); err != nil {
		t.Fatalf("failed to seed key hash: %v", err)
	}

	group := &models.Group{
		ID: key.GroupID,
		Config: datatypes.JSONMap{
			"blacklist_threshold":           5,
			"blacklist_window_minutes":      30,
			"consecutive_failure_threshold": 2,
		},
	}
	group.EffectiveConfig = provider.settingsManager.GetEffectiveConfig(group.Config)

	if err := provider.handleFailure(&models.APIKey{ID: key.ID}, group, keyHashKey, 1); err != nil {
		t.Fatalf("handleFailure returned error: %v", err)
	}

	var updated models.APIKey
	if err := db.First(&updated, key.ID).Error; err != nil {
		t.Fatalf("failed to reload key: %v", err)
	}
	if updated.Status != models.KeyStatusInvalid {
		t.Fatalf("expected key to be invalid after consecutive failures, got %s", updated.Status)
	}
	if updated.FailureCount != 1 {
		t.Fatalf("expected failure count snapshot to be 1, got %v", updated.FailureCount)
	}
}

func TestHandleRequestSuccessResetsConsecutiveFailuresOnly(t *testing.T) {
	provider, db, dataStore := newTestKeyProviderWithSettings(t)

	key := models.APIKey{
		GroupID:      13,
		KeyValue:     "success-key",
		KeyHash:      "hash-success",
		Status:       models.KeyStatusActive,
		Priority:     models.DefaultAPIKeyPriority,
		FailureCount: 1.5,
		CreatedAt:    time.Now(),
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("failed to create key: %v", err)
	}

	keyHashKey := fmt.Sprintf("key:%d", key.ID)
	if err := dataStore.HSet(keyHashKey, map[string]any{
		"status":                    models.KeyStatusActive,
		"failure_count":             1.5,
		"consecutive_failure_count": 2,
		"priority":                  models.DefaultAPIKeyPriority,
	}); err != nil {
		t.Fatalf("failed to seed key hash: %v", err)
	}
	seedFailureWindow(t, dataStore, key.ID, []failureWindowEntry{
		{Timestamp: time.Now().Add(-2 * time.Minute).Unix(), Penalty: 1},
		{Timestamp: time.Now().Add(-1 * time.Minute).Unix(), Penalty: 0.5},
	})

	group := &models.Group{
		ID: key.GroupID,
		Config: datatypes.JSONMap{
			"blacklist_threshold":      5,
			"blacklist_window_minutes": 30,
		},
	}
	group.EffectiveConfig = provider.settingsManager.GetEffectiveConfig(group.Config)

	if err := provider.handleRequestSuccess(key.ID, group, keyHashKey); err != nil {
		t.Fatalf("handleRequestSuccess returned error: %v", err)
	}

	var updated models.APIKey
	if err := db.First(&updated, key.ID).Error; err != nil {
		t.Fatalf("failed to reload key: %v", err)
	}
	if updated.FailureCount != 1.5 {
		t.Fatalf("expected rolling failure snapshot to stay at 1.5, got %v", updated.FailureCount)
	}

	keyDetails, err := dataStore.HGetAll(keyHashKey)
	if err != nil {
		t.Fatalf("failed to reload key hash: %v", err)
	}
	if got := parseConsecutiveFailureCount(keyDetails["consecutive_failure_count"]); got != 0 {
		t.Fatalf("expected consecutive failure count to reset to 0, got %d", got)
	}

	exists, err := dataStore.Exists(failureWindowStoreKey(key.ID))
	if err != nil {
		t.Fatalf("failed to check failure window: %v", err)
	}
	if !exists {
		t.Fatal("expected failure window to be preserved after request success")
	}
}
