package channel

import (
	"context"

	"gorm.io/datatypes"
)

type ValidationRequestOptions struct {
	ProbeParamOverrides datatypes.JSONMap
}

type validationRequestOptionsKey struct{}

func WithValidationRequestOptions(ctx context.Context, options ValidationRequestOptions) context.Context {
	return context.WithValue(ctx, validationRequestOptionsKey{}, options)
}

func getValidationRequestOptions(ctx context.Context) (ValidationRequestOptions, bool) {
	options, ok := ctx.Value(validationRequestOptionsKey{}).(ValidationRequestOptions)
	return options, ok
}
