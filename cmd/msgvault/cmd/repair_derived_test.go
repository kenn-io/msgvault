package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/rederive"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestFormatRepairDerivedSummaryReportsMessageMetadata(t *testing.T) {
	got := formatRepairDerivedSummary("discord/example", &rederive.Summary{
		MessagesScanned:          3,
		MessageMetadataRewritten: 2,
		AttachmentsTagged:        4,
		Duration:                 1250 * time.Millisecond,
	})
	assert.Equal(t,
		"discord/example: 3 messages scanned, 2 message metadata rewritten, 0 bodies rewritten, 4 attachments tagged (1s)\n",
		got,
	)
}

func TestRepairDerivedTargetsRejectsUnknownSourceType(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("gmail", "inbox@example.net")
	require.NoError(err)
	saved := repairDerivedSourceTypes
	t.Cleanup(func() { repairDerivedSourceTypes = saved })

	repairDerivedSourceTypes = []string{"gmial"}
	_, err = repairDerivedTargets(st)
	require.ErrorContains(err, `no re-derivation pass for source type "gmial"`)

	repairDerivedSourceTypes = []string{"gmail"}
	targets, err := repairDerivedTargets(st)
	require.NoError(err)
	require.Len(targets, 1)
}
