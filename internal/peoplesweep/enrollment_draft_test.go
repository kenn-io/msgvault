package peoplesweep

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnrollmentDraftsBindOwnerAndExpire(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	now := time.Date(2026, time.September, 23, 5, 0, 0, 0, time.UTC)
	drafts := NewEnrollmentDrafts(func() time.Time { return now })
	draft, err := drafts.Create("browser-session-a", "codex")
	requireChecks.NoError(err)
	assertChecks.NotEmpty(draft.ID)
	assertChecks.Empty(draft.Model)
	assertChecks.Equal(now.Add(EnrollmentDraftLifetime), draft.ExpiresAt)
	_, err = drafts.Get("browser-session-b", draft.ID)
	requireChecks.ErrorIs(err, ErrEnrollmentDraftNotFound)
	_, err = drafts.Create("browser-session-a", "codex")
	requireChecks.ErrorIs(err, ErrEnrollmentDraftActive)

	now = now.Add(EnrollmentDraftLifetime + time.Second)
	_, err = drafts.Get("browser-session-a", draft.ID)
	requireChecks.ErrorIs(err, ErrEnrollmentDraftNotFound)
	second, err := drafts.Create("browser-session-a", "codex")
	requireChecks.NoError(err)
	assertChecks.NotEqual(draft.ID, second.ID)
}

func TestEnrollmentDraftCancellationIsOwnerBound(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	drafts := NewEnrollmentDrafts(time.Now)
	draft, err := drafts.Create("terminal-a", "codex")
	requireChecks.NoError(err)
	requireChecks.ErrorIs(drafts.Cancel("terminal-b", draft.ID), ErrEnrollmentDraftNotFound)
	_, err = drafts.Get("terminal-a", draft.ID)
	requireChecks.NoError(err)
	requireChecks.NoError(drafts.Cancel("terminal-a", draft.ID))
	_, err = drafts.Get("terminal-a", draft.ID)
	requireChecks.ErrorIs(err, ErrEnrollmentDraftNotFound)
	_, err = drafts.Create("terminal-a", "codex")
	assertChecks.NoError(err)
}
