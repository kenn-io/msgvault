package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gcal"
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

func TestScheduledCalendarSyncRunsGoogleWhenMicrosoftFails(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	for sourceType, id := range map[string]string{mscalendar.SourceType: "cal-ms", sourceTypeCalendar: "primary"} {
		src, err := st.GetOrCreateSource(sourceType, "user@example.com/"+id)
		requirements.NoError(err)
		requirements.NoError(st.UpdateSourceSyncConfig(src.ID, `{"account_email":"user@example.com","calendar_id":"`+id+`"}`))
	}
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Microsoft.ClientID = "client-id"
	ctx := withInvocation(t.Context(), testInvocationWithConfig(cfg))
	googleErr := errors.New("google unavailable")
	googleCalled := false
	buildClient := func(context.Context, string, string, bool, ...bool) (gcal.API, error) {
		googleCalled = true
		return nil, googleErr
	}

	// The Microsoft calendar has no token, so its sync fails.
	err := runConfiguredGCalSyncWithClientBuilder(ctx, st, config.GCalSource{Email: "user@example.com"}, buildClient)
	assert.True(t, googleCalled, "the Google calendars still sync")
	requirements.ErrorIs(err, googleErr)
	assert.NotEqual(t, googleErr.Error(), err.Error(), "the Microsoft failure is reported too")
}
