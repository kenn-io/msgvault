package deletion

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func TestSourceReferenceForTargets(t *testing.T) {
	tests := []struct {
		name    string
		targets []query.DeletionTarget
		want    SourceReference
		wantErr error
	}{
		{name: "empty", wantErr: ErrNoDeletionTargets},
		{
			name: "incomplete",
			targets: []query.DeletionTarget{
				{MessageID: 1, SourceID: 1, SourceIdentifier: "user@example.invalid", SourceMessageID: "remote-1"},
			},
			wantErr: ErrIncompleteDeletionSource,
		},
		{
			name: "multiple sources",
			targets: []query.DeletionTarget{
				{MessageID: 1, SourceID: 1, SourceType: "gmail", SourceIdentifier: "user@example.invalid", SourceMessageID: "remote-1"},
				{MessageID: 2, SourceID: 2, SourceType: "imap", SourceIdentifier: "user@example.invalid", SourceMessageID: "remote-2"},
			},
			wantErr: ErrMultipleDeletionSources,
		},
		{
			name: "one source",
			targets: []query.DeletionTarget{
				{MessageID: 1, SourceID: 7, SourceType: "gmail", SourceIdentifier: "user@example.invalid", SourceMessageID: "remote-1"},
				{MessageID: 2, SourceID: 7, SourceType: "gmail", SourceIdentifier: "user@example.invalid", SourceMessageID: "remote-2"},
			},
			want: SourceReference{ID: 7, Type: "gmail", Identifier: "user@example.invalid"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SourceReferenceForTargets(tt.targets)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSourceMessageIDs(t *testing.T) {
	targets := []query.DeletionTarget{
		{SourceMessageID: "remote-2"},
		{SourceMessageID: "remote-1"},
	}

	assert.Equal(t, []string{"remote-2", "remote-1"}, SourceMessageIDs(targets))
}
