package proxy

import (
	"testing"

	"gpt-load/internal/models"
	"gpt-load/internal/types"

	"gorm.io/datatypes"
)

func TestResolveStreamFirstVisibleTimeoutSeconds(t *testing.T) {
	group := &models.Group{
		EffectiveConfig: types.SystemSettings{
			StreamFirstVisibleTimeoutSeconds: 120,
		},
		StreamTimeoutRules: datatypes.JSONMap{
			"gemini-2.5-flash": 30,
			"gemini-2.5-pro*":  180,
			"gemini-*":         90,
		},
	}

	if timeout := resolveStreamFirstVisibleTimeoutSeconds(group, "gemini-2.5-flash"); timeout != 30 {
		t.Fatalf("expected exact match timeout 30, got %d", timeout)
	}
	if timeout := resolveStreamFirstVisibleTimeoutSeconds(group, "gemini-2.5-pro-exp-03-25"); timeout != 180 {
		t.Fatalf("expected longest prefix timeout 180, got %d", timeout)
	}
	if timeout := resolveStreamFirstVisibleTimeoutSeconds(group, "gemini-3.0-flash"); timeout != 90 {
		t.Fatalf("expected wildcard prefix timeout 90, got %d", timeout)
	}
	if timeout := resolveStreamFirstVisibleTimeoutSeconds(group, "claude-3-7-sonnet"); timeout != 120 {
		t.Fatalf("expected default timeout 120, got %d", timeout)
	}
}
