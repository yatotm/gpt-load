package services

import (
	"fmt"
	"testing"
	"time"

	"gpt-load/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func openServicesTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := fmt.Sprintf("file:%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}

	if err := db.AutoMigrate(&models.APIKey{}, &models.RequestLog{}); err != nil {
		t.Fatalf("failed to migrate schema: %v", err)
	}

	return db
}

func TestListKeysInGroupQueryUsesStableOrderWithinPriority(t *testing.T) {
	db := openServicesTestDB(t)
	service := &KeyService{DB: db}

	now := time.Now()
	earlier := now.Add(-time.Hour)

	keys := []*models.APIKey{
		{GroupID: 1, KeyValue: "key-low", KeyHash: "hash-low", Status: models.KeyStatusActive, Priority: 10, LastUsedAt: &now},
		{GroupID: 1, KeyValue: "key-mid", KeyHash: "hash-mid", Status: models.KeyStatusActive, Priority: 10},
		{GroupID: 1, KeyValue: "key-high", KeyHash: "hash-high", Status: models.KeyStatusActive, Priority: 10, LastUsedAt: &earlier},
		{GroupID: 1, KeyValue: "key-top", KeyHash: "hash-top", Status: models.KeyStatusActive, Priority: 5},
	}

	for _, key := range keys {
		if err := db.Create(key).Error; err != nil {
			t.Fatalf("failed to create key: %v", err)
		}
	}

	var got []models.APIKey
	if err := service.ListKeysInGroupQuery(1, "", "").Find(&got).Error; err != nil {
		t.Fatalf("failed to query keys: %v", err)
	}

	if len(got) != len(keys) {
		t.Fatalf("unexpected key count: got %d want %d", len(got), len(keys))
	}

	wantOrder := []uint{keys[3].ID, keys[0].ID, keys[1].ID, keys[2].ID}
	for index, wantID := range wantOrder {
		if got[index].ID != wantID {
			t.Fatalf("unexpected key order at index %d: got %d want %d", index, got[index].ID, wantID)
		}
	}
}
