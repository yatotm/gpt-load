package requestoverride

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const (
	ModeSet    = "set"
	ModeRemove = "remove"
)

type Operation struct {
	Mode  string `json:"mode"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

type Spec struct {
	Operations []Operation `json:"operations"`
}

func Normalize(raw map[string]any) (map[string]any, error) {
	if raw == nil {
		return nil, nil
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}

	spec, err := Parse(raw)
	if err != nil {
		return nil, err
	}

	return spec.toMap()
}

func Merge(base, override map[string]any) (map[string]any, error) {
	if len(base) == 0 {
		return Normalize(override)
	}
	if len(override) == 0 {
		return Normalize(base)
	}

	baseSpec, err := Parse(base)
	if err != nil {
		return nil, err
	}
	overrideSpec, err := Parse(override)
	if err != nil {
		return nil, err
	}

	merged := Spec{
		Operations: make([]Operation, 0, len(baseSpec.Operations)+len(overrideSpec.Operations)),
	}
	merged.Operations = append(merged.Operations, baseSpec.Operations...)
	merged.Operations = append(merged.Operations, overrideSpec.Operations...)
	return merged.toMap()
}

func Parse(raw map[string]any) (Spec, error) {
	if len(raw) == 0 {
		return Spec{}, nil
	}

	for key := range raw {
		if key != "operations" {
			return Spec{}, fmt.Errorf("unsupported top-level field '%s'", key)
		}
	}

	var spec Spec
	payload, err := json.Marshal(raw)
	if err != nil {
		return Spec{}, fmt.Errorf("failed to marshal override spec: %w", err)
	}
	if err := json.Unmarshal(payload, &spec); err != nil {
		return Spec{}, fmt.Errorf("failed to parse override spec: %w", err)
	}

	for i := range spec.Operations {
		op := &spec.Operations[i]
		op.Mode = strings.ToLower(strings.TrimSpace(op.Mode))
		op.Path = strings.TrimSpace(op.Path)

		switch op.Mode {
		case ModeSet, ModeRemove:
		default:
			return Spec{}, fmt.Errorf("operation %d has unsupported mode '%s'", i, op.Mode)
		}

		if op.Path == "" {
			return Spec{}, fmt.Errorf("operation %d has empty path", i)
		}
		if _, err := splitPath(op.Path); err != nil {
			return Spec{}, fmt.Errorf("operation %d has invalid path: %w", i, err)
		}
	}

	return spec, nil
}

func ApplyDocument(document map[string]any, raw map[string]any) (map[string]any, error) {
	if len(raw) == 0 {
		return document, nil
	}

	spec, err := Parse(raw)
	if err != nil {
		return nil, err
	}

	return ApplySpec(document, spec)
}

func ApplySpec(document map[string]any, spec Spec) (map[string]any, error) {
	root := any(document)

	for i := range spec.Operations {
		path, err := splitPath(spec.Operations[i].Path)
		if err != nil {
			return nil, err
		}

		root, err = apply(root, path, spec.Operations[i])
		if err != nil {
			return nil, fmt.Errorf("failed to apply operation %d: %w", i, err)
		}
	}

	updated, ok := root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("document root must remain an object")
	}

	return updated, nil
}

func (s Spec) toMap() (map[string]any, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal normalized override spec: %w", err)
	}

	var normalized map[string]any
	if err := json.Unmarshal(payload, &normalized); err != nil {
		return nil, fmt.Errorf("failed to unmarshal normalized override spec: %w", err)
	}

	return normalized, nil
}

func splitPath(path string) ([]string, error) {
	parts := strings.Split(strings.TrimSpace(path), ".")
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty path")
	}

	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("path contains empty segment")
		}
		segments = append(segments, part)
	}

	return segments, nil
}

func apply(node any, path []string, op Operation) (any, error) {
	if len(path) == 0 {
		return node, fmt.Errorf("empty path")
	}

	segment := path[0]
	if len(path) == 1 {
		return applyLeaf(node, segment, op)
	}

	switch current := node.(type) {
	case map[string]any:
		child, exists := current[segment]
		if !exists || child == nil {
			if op.Mode == ModeRemove {
				return current, nil
			}
			child = newContainer(path[1])
		}

		updated, err := apply(child, path[1:], op)
		if err != nil {
			return nil, err
		}
		current[segment] = updated
		return current, nil
	case []any:
		index, err := parseIndex(segment)
		if err != nil {
			return nil, err
		}
		if index >= len(current) {
			if op.Mode == ModeRemove {
				return current, nil
			}
			current = ensureIndex(current, index)
		}

		child := current[index]
		if child == nil {
			if op.Mode == ModeRemove {
				return current, nil
			}
			child = newContainer(path[1])
		}

		updated, err := apply(child, path[1:], op)
		if err != nil {
			return nil, err
		}
		current[index] = updated
		return current, nil
	default:
		return nil, fmt.Errorf("path '%s' traverses a non-container value", strings.Join(path, "."))
	}
}

func applyLeaf(node any, segment string, op Operation) (any, error) {
	switch current := node.(type) {
	case map[string]any:
		if op.Mode == ModeRemove {
			delete(current, segment)
			return current, nil
		}
		current[segment] = op.Value
		return current, nil
	case []any:
		index, err := parseIndex(segment)
		if err != nil {
			return nil, err
		}
		if op.Mode == ModeRemove {
			if index >= len(current) {
				return current, nil
			}
			return append(current[:index], current[index+1:]...), nil
		}

		if index >= len(current) {
			current = ensureIndex(current, index)
		}
		current[index] = op.Value
		return current, nil
	default:
		return nil, fmt.Errorf("path '%s' targets a non-container value", segment)
	}
}

func parseIndex(segment string) (int, error) {
	index, err := strconv.Atoi(segment)
	if err != nil || index < 0 {
		return 0, fmt.Errorf("segment '%s' is not a valid array index", segment)
	}
	return index, nil
}

func newContainer(nextSegment string) any {
	if _, err := strconv.Atoi(nextSegment); err == nil {
		return []any{}
	}
	return map[string]any{}
}

func ensureIndex(items []any, index int) []any {
	if index < len(items) {
		return items
	}

	expanded := make([]any, index+1)
	copy(expanded, items)
	return expanded
}
