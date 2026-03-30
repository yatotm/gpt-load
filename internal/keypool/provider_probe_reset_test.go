package keypool

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/encryption"
	"gpt-load/internal/models"
	"gpt-load/internal/store"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newTestKeyProvider(t *testing.T) (*KeyProvider, *gorm.DB, store.Store) {
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
	provider := NewProvider(db, memStore, nil, encryptionSvc)
	return provider, db, memStore
}

func TestHandleSuccessClearsProbeWindowWhenRecoveringKey(t *testing.T) {
	provider, db, dataStore := newTestKeyProvider(t)

	key := models.APIKey{
		GroupID:          1,
		KeyValue:         "probe-key",
		KeyHash:          "hash-1",
		Status:           models.KeyStatusInvalid,
		Priority:         models.DefaultAPIKeyPriority,
		FailureCount:     3,
		ProbeFailureRate: 75,
		ProbeSampleCount: 4,
		CreatedAt:        time.Now(),
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("failed to create key: %v", err)
	}
	keyHashKey := fmt.Sprintf("key:%d", key.ID)
	if err := dataStore.HSet(keyHashKey, map[string]any{
		"status":        models.KeyStatusInvalid,
		"failure_count": 3,
		"priority":      models.DefaultAPIKeyPriority,
	}); err != nil {
		t.Fatalf("failed to seed key hash: %v", err)
	}
	if err := dataStore.Set(probeWindowStoreKey(key.ID), []byte(`[{"timestamp":1,"success":false}]`), time.Minute); err != nil {
		t.Fatalf("failed to seed probe window: %v", err)
	}

	if err := provider.handleSuccess(key.ID, keyHashKey); err != nil {
		t.Fatalf("handleSuccess returned error: %v", err)
	}

	var updated models.APIKey
	if err := db.First(&updated, key.ID).Error; err != nil {
		t.Fatalf("failed to reload key: %v", err)
	}
	if updated.Status != models.KeyStatusActive {
		t.Fatalf("expected key to be active, got %s", updated.Status)
	}
	if updated.ProbeFailureRate != 0 || updated.ProbeSampleCount != 0 {
		t.Fatalf("expected probe stats to be reset, got rate=%v sample=%d", updated.ProbeFailureRate, updated.ProbeSampleCount)
	}

	exists, err := dataStore.Exists(probeWindowStoreKey(key.ID))
	if err != nil {
		t.Fatalf("failed to check probe window: %v", err)
	}
	if exists {
		t.Fatal("expected probe window to be deleted")
	}
}

func TestRestoreMultipleKeysClearsProbeStatsAndWindow(t *testing.T) {
	provider, db, dataStore := newTestKeyProvider(t)

	key := models.APIKey{
		GroupID:          9,
		KeyValue:         "restore-key",
		KeyHash:          provider.encryptionSvc.Hash("restore-key"),
		Status:           models.KeyStatusInvalid,
		Priority:         models.DefaultAPIKeyPriority,
		FailureCount:     2,
		ProbeFailureRate: 50,
		ProbeSampleCount: 6,
		CreatedAt:        time.Now(),
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("failed to create key: %v", err)
	}
	if err := dataStore.Set(probeWindowStoreKey(key.ID), []byte(`[{"timestamp":1,"success":false}]`), time.Minute); err != nil {
		t.Fatalf("failed to seed probe window: %v", err)
	}

	restored, err := provider.RestoreMultipleKeys(key.GroupID, []string{"restore-key"})
	if err != nil {
		t.Fatalf("RestoreMultipleKeys returned error: %v", err)
	}
	if restored != 1 {
		t.Fatalf("expected 1 restored key, got %d", restored)
	}

	var updated models.APIKey
	if err := db.First(&updated, key.ID).Error; err != nil {
		t.Fatalf("failed to reload key: %v", err)
	}
	if updated.Status != models.KeyStatusActive {
		t.Fatalf("expected key to be active, got %s", updated.Status)
	}
	if updated.ProbeFailureRate != 0 || updated.ProbeSampleCount != 0 {
		t.Fatalf("expected probe stats to be reset, got rate=%v sample=%d", updated.ProbeFailureRate, updated.ProbeSampleCount)
	}

	exists, err := dataStore.Exists(probeWindowStoreKey(key.ID))
	if err != nil {
		t.Fatalf("failed to check probe window: %v", err)
	}
	if exists {
		t.Fatal("expected probe window to be deleted")
	}
}
