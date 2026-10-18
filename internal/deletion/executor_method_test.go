package deletion

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteOneRejectsUnknownMethod(t *testing.T) {
	for _, method := range []Method{"", "purge-v9", "TRASH", " trash "} {
		t.Run(fmt.Sprintf("%q", method), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			tc := NewTestContext(t)
			tc.SeedMessages([]string{"msg-1"})
			source, err := tc.Store.GetOrCreateSource("gmail", "test@example.com")
			require.NoError(err)

			result, err := tc.Exec.deleteOne(t.Context(), source.ID, "msg-1", method)

			require.ErrorContains(err, fmt.Sprintf("unsupported deletion method %q", method))
			assert.Equal(resultFatal, result, "an unknown method must halt execution")
			assert.Empty(tc.MockAPI.DeleteCalls)
			assert.Empty(tc.MockAPI.TrashCalls)
			assert.Empty(tc.MockAPI.BatchDeleteCalls)
			assert.Zero(tc.CountDeleted(), "rejection must not mark the archived message deleted")
		})
	}
}

func TestDeleteOneAcceptsKnownMethods(t *testing.T) {
	for _, method := range []Method{MethodTrash, MethodDelete} {
		t.Run(string(method), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			tc := NewTestContext(t)
			tc.SeedMessages([]string{"msg-1"})
			source, err := tc.Store.GetOrCreateSource("gmail", "test@example.com")
			require.NoError(err)

			result, err := tc.Exec.deleteOne(t.Context(), source.ID, "msg-1", method)

			require.NoError(err)
			assert.Equal(resultSuccess, result)
			if method == MethodTrash {
				assert.Equal([]string{"msg-1"}, tc.MockAPI.TrashCalls)
				assert.Empty(tc.MockAPI.DeleteCalls)
			} else {
				assert.Equal([]string{"msg-1"}, tc.MockAPI.DeleteCalls)
				assert.Empty(tc.MockAPI.TrashCalls)
			}
			assert.Empty(tc.MockAPI.BatchDeleteCalls)
			assert.Equal(1, tc.CountDeleted())
		})
	}
}

func TestExecuteRejectsUnknownMethodBeforeClaim(t *testing.T) {
	for _, method := range []Method{"", "purge-v9", "TRASH", " trash "} {
		t.Run(fmt.Sprintf("%q", method), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			tc := NewTestContext(t)
			tc.SeedMessages([]string{"msg-1"})
			manifest := tc.CreateManifest("unknown-method", []string{"msg-1"})
			before, status, err := tc.Mgr.GetManifestWithStatus(manifest.ID)
			require.NoError(err)
			require.Equal(StatusPending, status)
			require.Nil(before.Execution)

			err = tc.Exec.Execute(t.Context(), manifest.ID, &ExecuteOptions{Method: method, BatchSize: 100})

			require.ErrorContains(err, fmt.Sprintf("unsupported deletion method %q", method))
			assert.Empty(tc.MockAPI.DeleteCalls)
			assert.Empty(tc.MockAPI.TrashCalls)
			assert.Empty(tc.MockAPI.BatchDeleteCalls)
			assert.Zero(tc.CountDeleted())
			after, status, err := tc.Mgr.GetManifestWithStatus(manifest.ID)
			require.NoError(err)
			assert.Equal(StatusPending, status)
			assert.Nil(after.Execution)
			assert.Equal(before, after, "rejection must leave the pending manifest unchanged")
		})
	}
}
