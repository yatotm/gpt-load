package keypool

import (
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

	if err := provider.handleFailure(&models.APIKey{ID: key.ID}, group, keyHashKey); err != nil {
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
		t.Fatalf("expected failure count to increase to 1, got %d", updated.FailureCount)
	}
}
