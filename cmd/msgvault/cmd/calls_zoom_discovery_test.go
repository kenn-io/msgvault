package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/zoomcalls"
)

func TestZoomDiscoveryValidation(t *testing.T) { assertZoomNeedsToken(t, "import-zoom-meetings") }

func TestZoomDiscoveryReconciliationBudgetIsPerRun(t *testing.T) {
	client := &zoomMetadataClient{}
	items := []zoomcalls.MeetingRecord{{UUID: "first", Roster: []byte(`{"participants":[]}`)}, {UUID: "second", Roster: []byte(`{"participants":[]}`)}}
	_, _, err := stageZoomMeetings(t.Context(), client, zoomOptions{account: "account", owner: "owner"}, items, nil)
	require.ErrorContains(t, err, "10000")
	assert.Equal(t, 100, client.calls)
}
