package services

import (
	"strings"
	"testing"

	"gpt-load/internal/models"
)

func TestASCIISafeStringEscapesNonASCII(t *testing.T) {
	got := asciiSafeString("A你\t🙂")
	want := "A\\u4F60\t\\U0001F642"
	if got != want {
		t.Fatalf("unexpected escaped string: got %q want %q", got, want)
	}
}

func TestSanitizeRequestLogForLegacyCharsetTruncatesFields(t *testing.T) {
	logEntry := &models.RequestLog{
		GroupName:       strings.Repeat("组", 200),
		ParentGroupName: strings.Repeat("父", 200),
		Model:           strings.Repeat("模", 200),
		EffectiveModel:  strings.Repeat("型", 200),
		SourceIP:        strings.Repeat("地", 100),
		RequestPath:     strings.Repeat("路", 200),
		UserAgent:       strings.Repeat("端", 200),
		UpstreamAddr:    strings.Repeat("上", 200),
		RequestBody:     "请求体中文",
		ErrorMessage:    "错误信息中文",
	}

	sanitizeRequestLogForLegacyCharset(logEntry)

	if len(logEntry.GroupName) > 255 {
		t.Fatalf("group_name should be truncated to 255, got %d", len(logEntry.GroupName))
	}
	if len(logEntry.ParentGroupName) > 255 {
		t.Fatalf("parent_group_name should be truncated to 255, got %d", len(logEntry.ParentGroupName))
	}
	if len(logEntry.Model) > 255 {
		t.Fatalf("model should be truncated to 255, got %d", len(logEntry.Model))
	}
	if len(logEntry.EffectiveModel) > 255 {
		t.Fatalf("effective_model should be truncated to 255, got %d", len(logEntry.EffectiveModel))
	}
	if len(logEntry.SourceIP) > 64 {
		t.Fatalf("source_ip should be truncated to 64, got %d", len(logEntry.SourceIP))
	}
	if len(logEntry.RequestPath) > 500 {
		t.Fatalf("request_path should be truncated to 500, got %d", len(logEntry.RequestPath))
	}
	if len(logEntry.UserAgent) > 512 {
		t.Fatalf("user_agent should be truncated to 512, got %d", len(logEntry.UserAgent))
	}
	if len(logEntry.UpstreamAddr) > 500 {
		t.Fatalf("upstream_addr should be truncated to 500, got %d", len(logEntry.UpstreamAddr))
	}
	if strings.Contains(logEntry.RequestBody, "中") {
		t.Fatalf("request_body should be ASCII-safe after sanitizing, got %q", logEntry.RequestBody)
	}
	if strings.Contains(logEntry.ErrorMessage, "中") {
		t.Fatalf("error_message should be ASCII-safe after sanitizing, got %q", logEntry.ErrorMessage)
	}
}
