package cmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
)

func TestMetaWebhookErrorRetriesOnlyRecoverableFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
	}{
		{"source busy", fmt.Errorf("apply: %w", store.ErrSyncAlreadyActive), false},
		{"Graph unavailable", &meta.APIError{Status: 503}, false},
		{"Graph throttled with 400", &meta.APIError{Status: 400, Code: 4}, false},
		{"Graph throttled with 429", &meta.APIError{Status: 429}, false},
		{"post deleted", fmt.Errorf("read post: %w", &meta.APIError{Status: 400, Code: 100, Subcode: 33}), true},
		{"source not registered", store.ErrSourceNotFound, true},
		{"invalid change", fmt.Errorf("%w: bad", meta.ErrInvalidChange), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := metaWebhookError(tc.err)
			require.ErrorIs(t, err, tc.err)
			assert.Equal(t, tc.permanent, errors.Is(err, metawebhook.ErrUnrecoverable))
		})
	}
}
