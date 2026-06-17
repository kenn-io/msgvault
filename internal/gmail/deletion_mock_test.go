package gmail

import (
	"context"
	"errors"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func setupDeletionMockTest(t *testing.T) (*DeletionMockAPI, context.Context) {
	t.Helper()
	return NewDeletionMockAPI(), context.Background()
}

func assertCallSequence(t *testing.T, mock *DeletionMockAPI, expectedOps ...string) {
	t.Helper()
	requirepkg.Len(t, mock.CallSequence, len(expectedOps), "CallSequence length")
	for i, want := range expectedOps {
		assertpkg.Equal(t, want, mock.CallSequence[i].Operation, "CallSequence[%d].Operation", i)
	}
}

func TestDeletionMockAPI_CallSequence(t *testing.T) {
	mockAPI, ctx := setupDeletionMockTest(t)

	_ = mockAPI.TrashMessage(ctx, "msg1")
	_ = mockAPI.DeleteMessage(ctx, "msg2")
	_ = mockAPI.BatchDeleteMessages(ctx, []string{"msg3", "msg4"})

	assertCallSequence(t, mockAPI, "trash", "delete", "batch_delete")
}

func TestDeletionMockAPI_Reset(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	mockAPI, ctx := setupDeletionMockTest(t)

	// Dirty all trackable fields with successful calls
	_ = mockAPI.TrashMessage(ctx, "msg1")
	_ = mockAPI.DeleteMessage(ctx, "msg2")
	_ = mockAPI.BatchDeleteMessages(ctx, []string{"msg3"})

	// Also set error maps to verify they get cleared
	mockAPI.TrashErrors["msg-err"] = errors.New("error")
	mockAPI.DeleteErrors["msg-err"] = errors.New("error")
	mockAPI.BatchDeleteError = errors.New("error")

	// Set transient failures to verify they get cleared
	mockAPI.TransientTrashFailures["msg-trans"] = 3
	mockAPI.TransientDeleteFailures["msg-trans"] = 2

	// Set rate limit fields to verify they get cleared
	mockAPI.RateLimitAfterCalls = 10
	mockAPI.RateLimitDuration = 5

	// Set hooks to verify they get cleared
	hookCalled := false
	mockAPI.BeforeTrash = func(string) error { hookCalled = true; return nil }
	mockAPI.BeforeDelete = func(string) error { hookCalled = true; return nil }
	mockAPI.BeforeBatchDelete = func([]string) error { hookCalled = true; return nil }

	// Assert call-tracking data is populated before Reset
	require.NotEmpty(mockAPI.TrashCalls, "TrashCalls should be populated before Reset")
	require.NotEmpty(mockAPI.DeleteCalls, "DeleteCalls should be populated before Reset")
	require.NotEmpty(mockAPI.BatchDeleteCalls, "BatchDeleteCalls should be populated before Reset")
	require.NotEmpty(mockAPI.CallSequence, "CallSequence should be populated before Reset")

	mockAPI.Reset()

	assert.Empty(mockAPI.TrashErrors, "TrashErrors not cleared")
	assert.Empty(mockAPI.DeleteErrors, "DeleteErrors not cleared")
	require.NoError(mockAPI.BatchDeleteError, "BatchDeleteError not cleared")
	assert.Empty(mockAPI.TransientTrashFailures, "TransientTrashFailures not cleared")
	assert.Empty(mockAPI.TransientDeleteFailures, "TransientDeleteFailures not cleared")
	assert.Equal(0, mockAPI.RateLimitAfterCalls, "RateLimitAfterCalls not cleared")
	assert.Equal(0, mockAPI.RateLimitDuration, "RateLimitDuration not cleared")
	assert.Empty(mockAPI.TrashCalls, "TrashCalls not cleared")
	assert.Empty(mockAPI.DeleteCalls, "DeleteCalls not cleared")
	assert.Empty(mockAPI.BatchDeleteCalls, "BatchDeleteCalls not cleared")
	assert.Empty(mockAPI.CallSequence, "CallSequence not cleared")
	assert.Nil(mockAPI.BeforeTrash, "BeforeTrash not cleared")
	assert.Nil(mockAPI.BeforeDelete, "BeforeDelete not cleared")
	assert.Nil(mockAPI.BeforeBatchDelete, "BeforeBatchDelete not cleared")

	// Verify hooks are not invoked after Reset
	_ = mockAPI.TrashMessage(ctx, "after-reset")
	assert.False(hookCalled, "hook was invoked after Reset")
}

func TestDeletionMockAPI_GetCallCount(t *testing.T) {
	mockAPI, ctx := setupDeletionMockTest(t)

	_ = mockAPI.TrashMessage(ctx, "msg1")
	_ = mockAPI.TrashMessage(ctx, "msg1")
	_ = mockAPI.TrashMessage(ctx, "msg2")

	tests := []struct {
		msgID string
		want  int
	}{
		{"msg1", 2},
		{"msg2", 1},
		{"msg3", 0},
	}

	for _, tt := range tests {
		got := mockAPI.GetTrashCallCount(tt.msgID)
		assertpkg.Equal(t, tt.want, got, "GetTrashCallCount(%q)", tt.msgID)
	}
}

func TestDeletionMockAPI_Close(t *testing.T) {
	mockAPI, _ := setupDeletionMockTest(t)
	assertpkg.NoError(t, mockAPI.Close(), "Close()")
}

func TestDeletionMockAPI_Hooks(t *testing.T) {
	tests := []struct {
		name      string
		setupHook func(*DeletionMockAPI, *bool)
		act       func(context.Context, *DeletionMockAPI) error
		wantErr   bool
	}{
		{
			name: "BeforeTrash allow",
			setupHook: func(m *DeletionMockAPI, called *bool) {
				m.BeforeTrash = func(string) error { *called = true; return nil }
			},
			act:     func(ctx context.Context, m *DeletionMockAPI) error { return m.TrashMessage(ctx, "msg1") },
			wantErr: false,
		},
		{
			name: "BeforeTrash block",
			setupHook: func(m *DeletionMockAPI, called *bool) {
				m.BeforeTrash = func(string) error { *called = true; return errors.New("blocked") }
			},
			act:     func(ctx context.Context, m *DeletionMockAPI) error { return m.TrashMessage(ctx, "msg1") },
			wantErr: true,
		},
		{
			name: "BeforeDelete allow",
			setupHook: func(m *DeletionMockAPI, called *bool) {
				m.BeforeDelete = func(string) error { *called = true; return nil }
			},
			act:     func(ctx context.Context, m *DeletionMockAPI) error { return m.DeleteMessage(ctx, "msg1") },
			wantErr: false,
		},
		{
			name: "BeforeDelete block",
			setupHook: func(m *DeletionMockAPI, called *bool) {
				m.BeforeDelete = func(string) error { *called = true; return errors.New("blocked") }
			},
			act:     func(ctx context.Context, m *DeletionMockAPI) error { return m.DeleteMessage(ctx, "msg1") },
			wantErr: true,
		},
		{
			name: "BeforeBatchDelete allow",
			setupHook: func(m *DeletionMockAPI, called *bool) {
				m.BeforeBatchDelete = func([]string) error { *called = true; return nil }
			},
			act: func(ctx context.Context, m *DeletionMockAPI) error {
				return m.BatchDeleteMessages(ctx, []string{"msg1", "msg2"})
			},
			wantErr: false,
		},
		{
			name: "BeforeBatchDelete block",
			setupHook: func(m *DeletionMockAPI, called *bool) {
				m.BeforeBatchDelete = func([]string) error { *called = true; return errors.New("blocked") }
			},
			act: func(ctx context.Context, m *DeletionMockAPI) error {
				return m.BatchDeleteMessages(ctx, []string{"msg1", "msg2"})
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockAPI, ctx := setupDeletionMockTest(t)
			hookCalled := false
			tt.setupHook(mockAPI, &hookCalled)
			err := tt.act(ctx, mockAPI)
			assertpkg.True(t, hookCalled, "hook was not called")
			if tt.wantErr {
				assertpkg.Error(t, err)
			} else {
				assertpkg.NoError(t, err)
			}
		})
	}
}

func TestDeletionMockAPI_GetDeleteCallCount(t *testing.T) {
	mockAPI, ctx := setupDeletionMockTest(t)

	_ = mockAPI.DeleteMessage(ctx, "msg1")
	_ = mockAPI.DeleteMessage(ctx, "msg1")

	assertpkg.Equal(t, 2, mockAPI.GetDeleteCallCount("msg1"), "GetDeleteCallCount(msg1)")
}

func TestDeletionMockAPI_TransientFailures(t *testing.T) {
	tests := []struct {
		name       string
		failCount  int
		isTrash    bool
		callMethod func(context.Context, *DeletionMockAPI) error
	}{
		{
			name:       "TrashTransientFailure",
			failCount:  2,
			isTrash:    true,
			callMethod: func(ctx context.Context, m *DeletionMockAPI) error { return m.TrashMessage(ctx, "msg1") },
		},
		{
			name:       "DeleteTransientFailure",
			failCount:  1,
			isTrash:    false,
			callMethod: func(ctx context.Context, m *DeletionMockAPI) error { return m.DeleteMessage(ctx, "msg1") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockAPI, ctx := setupDeletionMockTest(t)
			mockAPI.SetTransientFailure("msg1", tt.failCount, tt.isTrash)

			for i := range tt.failCount {
				requirepkg.Error(t, tt.callMethod(ctx, mockAPI), "call %d should fail", i+1)
			}

			assertpkg.NoError(t, tt.callMethod(ctx, mockAPI), "call after failures should succeed")
		})
	}
}
