package keypool

import (
	"errors"
	"fmt"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/models"
	"testing"
	"time"

	"gorm.io/datatypes"
)

func TestUpdateKeyMetaStatusRemovesPausedKeyFromSelectionAndRestoresOnActive(t *testing.T) {
	provider, db, _ := newTestKeyProvider(t)

	key := models.APIKey{
		GroupID:   21,
		KeyValue:  "pause-me",
		KeyHash:   "hash-pause-me",
		Status:    models.KeyStatusActive,
		Priority:  models.DefaultAPIKeyPriority,
		CreatedAt: time.Now(),
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("failed to create key: %v", err)
	}
	if err := provider.addKeyToStore(&key); err != nil {
		t.Fatalf("failed to seed key store: %v", err)
	}

	paused := models.KeyStatusPaused
	if _, err := provider.UpdateKeyMeta(key.ID, KeyMetaUpdate{Status: &paused}); err != nil {
		t.Fatalf("failed to pause key: %v", err)
	}

	_, err := provider.SelectKey(key.GroupID)
	if !errors.Is(err, app_errors.ErrNoActiveKeys) {
		t.Fatalf("expected no active keys after pause, got %v", err)
	}

	active := models.KeyStatusActive
	if _, err := provider.UpdateKeyMeta(key.ID, KeyMetaUpdate{Status: &active}); err != nil {
		t.Fatalf("failed to reactivate key: %v", err)
	}

	selected, err := provider.SelectKey(key.GroupID)
	if err != nil {
		t.Fatalf("expected key to be selectable after resume, got %v", err)
	}
	if selected.ID != key.ID {
		t.Fatalf("expected selected key %d, got %d", key.ID, selected.ID)
	}
}

func TestUpdateProbeStatusKeepsPausedKeyPaused(t *testing.T) {
	provider, db, dataStore := newTestKeyProvider(t)

	key := models.APIKey{
		GroupID:   22,
		KeyValue:  "paused-probe",
		KeyHash:   "hash-paused-probe",
		Status:    models.KeyStatusPaused,
		Priority:  models.DefaultAPIKeyPriority,
		CreatedAt: time.Now(),
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("failed to create key: %v", err)
	}

	keyHashKey := fmt.Sprintf("key:%d", key.ID)
	if err := dataStore.HSet(keyHashKey, map[string]any{
		"status":                    models.KeyStatusPaused,
		"failure_count":             0,
		"consecutive_failure_count": 0,
		"priority":                  models.DefaultAPIKeyPriority,
	}); err != nil {
		t.Fatalf("failed to seed key hash: %v", err)
	}

	checkedAt := time.Now()
	if err := provider.UpdateProbeStatus(&key, ProbeStatusUpdate{
		CheckedAt:    checkedAt,
		Success:      false,
		StatusCode:   429,
		ErrorMessage: "probe failed",
		FailureRate:  100,
		SampleCount:  6,
	}, true, true); err != nil {
		t.Fatalf("UpdateProbeStatus returned error: %v", err)
	}

	var updated models.APIKey
	if err := db.First(&updated, key.ID).Error; err != nil {
		t.Fatalf("failed to reload key: %v", err)
	}
	if updated.Status != models.KeyStatusPaused {
		t.Fatalf("expected paused key to remain paused, got %s", updated.Status)
	}
	if updated.LastProbeAt == nil || !updated.LastProbeAt.Equal(checkedAt) {
		t.Fatalf("expected last probe time to be updated")
	}
	if updated.ProbeFailureRate != 100 || updated.ProbeSampleCount != 6 {
		t.Fatalf("expected probe stats to be updated, got rate=%v sample=%d", updated.ProbeFailureRate, updated.ProbeSampleCount)
	}
}

func TestHandleFailureIgnoresPausedKey(t *testing.T) {
	provider, db, dataStore := newTestKeyProviderWithSettings(t)

	key := models.APIKey{
		GroupID:      23,
		KeyValue:     "paused-validation",
		KeyHash:      "hash-paused-validation",
		Status:       models.KeyStatusPaused,
		Priority:     models.DefaultAPIKeyPriority,
		FailureCount: 0,
		CreatedAt:    time.Now(),
	}
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("failed to create key: %v", err)
	}

	keyHashKey := fmt.Sprintf("key:%d", key.ID)
	if err := dataStore.HSet(keyHashKey, map[string]any{
		"status":                    models.KeyStatusPaused,
		"failure_count":             0,
		"consecutive_failure_count": 0,
		"priority":                  models.DefaultAPIKeyPriority,
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
	if updated.Status != models.KeyStatusPaused {
		t.Fatalf("expected paused key to remain paused, got %s", updated.Status)
	}
	if updated.FailureCount != 0 {
		t.Fatalf("expected paused key failure count to stay unchanged, got %v", updated.FailureCount)
	}
}
