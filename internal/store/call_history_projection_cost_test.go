package store_test

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCallSummaryReadsOnlyScalarEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	o, sourceID := validCallObservation(t, st)
	_, err := st.PutCallObservationContext(t.Context(), sourceID, o)
	require.NoError(err)

	logs := captureAttachmentQueryLogs(t)
	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true, MaxStmtChars: -1})
	_, err = st.SummarizeCallsContext(t.Context(), store.CallQuery{})
	require.NoError(err)
	// Legacy calls read connected seconds in SQL; the summary must still
	// avoid fetching whole JSON documents for decoding in Go.
	scalarReads := regexp.MustCompile(`(?i)json_valid\(m\.metadata\)|m\.metadata\s*->>\s*'call_connected_seconds'`)
	assert.NotEmpty(logs.String(), "the summary query must be captured")
	assert.NotContains(scalarReads.ReplaceAllString(logs.String(), ""), "metadata", "summary SQL must avoid fetching JSON evidence for decoding in Go")
}
