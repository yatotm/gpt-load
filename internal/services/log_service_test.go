package services

import (
	"net/http/httptest"
	"testing"
	"time"

	"gpt-load/internal/encryption"
	"gpt-load/internal/models"

	"github.com/gin-gonic/gin"
)

func TestGetLogsQueryIncludesKeyNote(t *testing.T) {
	db := openServicesTestDB(t)
	encryptionSvc, err := encryption.NewService("")
	if err != nil {
		t.Fatalf("failed to create encryption service: %v", err)
	}

	service := NewLogService(db, encryptionSvc)
	keyHash := encryptionSvc.Hash("sk-note")

	key := &models.APIKey{
		GroupID:   1,
		KeyValue:  "sk-note",
		KeyHash:   keyHash,
		Status:    models.KeyStatusActive,
		Priority:  100,
		Notes:     "主账号",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := db.Create(key).Error; err != nil {
		t.Fatalf("failed to create api key: %v", err)
	}

	logRecord := &models.RequestLog{
		ID:          "log-1",
		Timestamp:   time.Now(),
		GroupID:     1,
		GroupName:   "group-a",
		KeyValue:    "sk-note",
		KeyHash:     keyHash,
		Model:       "gpt-4o-mini",
		IsSuccess:   true,
		SourceIP:    "127.0.0.1",
		StatusCode:  200,
		RequestPath: "/v1/chat/completions",
		Duration:    123,
		RequestType: models.RequestTypeFinal,
	}
	if err := db.Create(logRecord).Error; err != nil {
		t.Fatalf("failed to create request log: %v", err)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("GET", "/logs", nil)

	var logs []models.RequestLog
	if err := service.GetLogsQuery(c).Find(&logs).Error; err != nil {
		t.Fatalf("failed to query logs: %v", err)
	}

	if len(logs) != 1 {
		t.Fatalf("unexpected log count: got %d want 1", len(logs))
	}
	if logs[0].KeyNote != "主账号" {
		t.Fatalf("unexpected key note: got %q want %q", logs[0].KeyNote, "主账号")
	}
}
