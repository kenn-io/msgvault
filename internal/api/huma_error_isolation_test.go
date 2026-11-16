package api

import (
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/require"
)

// A process can serve this API beside other Huma APIs. Building this one
// must leave their error responses alone. This test reads a package-level
// Huma variable, so it must not run in parallel.
func TestServerLeavesHumaErrorFactoryUntouched(t *testing.T) {
	original := huma.NewError
	t.Cleanup(func() { huma.NewError = original })
	originalPointer := reflect.ValueOf(original).Pointer()

	srv, _ := newTestServerWithMockStore(t)
	_ = srv.Router()

	require.Equal(t, originalPointer, reflect.ValueOf(huma.NewError).Pointer())
	_, ok := huma.NewError(400, "another API's error").(*huma.ErrorModel)
	require.True(t, ok, "another Huma API keeps its own error model")
}
