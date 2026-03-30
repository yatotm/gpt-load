package channel

import (
	"context"
	"encoding/json"
	"fmt"

	"gpt-load/internal/requestoverride"
)

func marshalValidationPayload(ctx context.Context, payload map[string]any) ([]byte, error) {
	options, ok := getValidationRequestOptions(ctx)
	if !ok || len(options.ProbeParamOverrides) == 0 {
		return json.Marshal(payload)
	}

	updated, err := requestoverride.ApplyDocument(payload, map[string]any(options.ProbeParamOverrides))
	if err != nil {
		return nil, fmt.Errorf("failed to apply probe param overrides: %w", err)
	}

	return json.Marshal(updated)
}
