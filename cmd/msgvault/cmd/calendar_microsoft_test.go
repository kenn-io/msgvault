package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/mscalendar"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMicrosoftCalendarSyncRejectsGoogleOnlyFlags(t *testing.T) {
	requirements := require.New(t)
	t.Cleanup(saveCalendarCommandGlobals())
	cmd := newSyncCalendarLocalCmd()
	requirements.NoError(rejectMSCalendarSyncFlags(cmd))
	requirements.NoError(cmd.Flags().Set("full", "true"))
	requirements.NoError(rejectMSCalendarSyncFlags(cmd), "a Microsoft sync is always full")
	requirements.NoError(cmd.Flags().Set("oauth-app", "acme"))
	requirements.EqualError(rejectMSCalendarSyncFlags(cmd), "--oauth-app is not supported for Microsoft calendars")
}

func TestMicrosoftCalendarAddRejectsGoogleOnlyFlags(t *testing.T) {
	requirements := require.New(t)
	t.Cleanup(saveCalendarCommandGlobals())
	cmd := newAddCalendarLocalCmd()
	requirements.NoError(rejectMSCalendarAddFlags(cmd))
	requirements.NoError(cmd.Flags().Set("calendars", "primary"))
	requirements.EqualError(rejectMSCalendarAddFlags(cmd), "--calendars is not supported with --microsoft")
}

func TestMicrosoftAccountStaysMicrosoftWhenItsSourcesAreMerged(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource(mscalendar.SourceType, "me@example.com/cal-1")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(from.ID, `{"account_email":"me@example.com","calendar_id":"cal-1"}`))
	into, err := st.GetOrCreateSource(mscalendar.SourceType, "archive@example.com/cal-1")
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	requirements.NoError(err)

	ok, err := hasMSCalendarSources(st, "me@example.com")
	requirements.NoError(err)
	assert.True(t, ok, "a merged-away account must not fall through to Google sign-in")
}
