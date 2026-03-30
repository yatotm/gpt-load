package requestoverride

import "testing"

func TestApplyDocumentSetNestedValue(t *testing.T) {
	document := map[string]any{
		"generationConfig": map[string]any{},
	}
	spec := map[string]any{
		"operations": []any{
			map[string]any{
				"mode":  "set",
				"path":  "generationConfig.thinkingConfig.thinkingLevel",
				"value": "minimal",
			},
		},
	}

	updated, err := ApplyDocument(document, spec)
	if err != nil {
		t.Fatalf("ApplyDocument returned error: %v", err)
	}

	generationConfig, ok := updated["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig is not an object")
	}
	thinkingConfig, ok := generationConfig["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("thinkingConfig is not an object")
	}
	if got := thinkingConfig["thinkingLevel"]; got != "minimal" {
		t.Fatalf("unexpected thinkingLevel: %v", got)
	}
}

func TestApplyDocumentRemoveMissingPathIsNoop(t *testing.T) {
	document := map[string]any{
		"temperature": 0.2,
	}
	spec := map[string]any{
		"operations": []any{
			map[string]any{
				"mode": "remove",
				"path": "generationConfig.thinkingConfig",
			},
		},
	}

	updated, err := ApplyDocument(document, spec)
	if err != nil {
		t.Fatalf("ApplyDocument returned error: %v", err)
	}

	if got := updated["temperature"]; got != 0.2 {
		t.Fatalf("unexpected temperature: %v", got)
	}
}

func TestNormalizeRejectsUnknownTopLevelField(t *testing.T) {
	_, err := Normalize(map[string]any{
		"foo": "bar",
	})
	if err == nil {
		t.Fatal("expected error for unsupported top-level field")
	}
}
