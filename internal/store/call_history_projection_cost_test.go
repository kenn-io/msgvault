package store_test

import (
	"bufio"
	"encoding/json/v2"
	"regexp"
	"strings"
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
	// The fixture selects SQLite or PostgreSQL through MSGVAULT_TEST_DB.
	// Inspect executed SQL because fetching unused JSON has the same result
	// as reading scalar evidence. Allow legacy scalar extraction in either dialect.
	scalarReads := regexp.MustCompile(`(?i)json_valid\s*\(\s*(?:\w+\s*\.\s*)?"?metadata"?\s*\)|(?:\w+\s*\.\s*)?"?metadata"?\s*->>\s*'call_connected_seconds'`)
	wholeEvidence := regexp.MustCompile(`(?i)\bmetadata\b`)
	statements := 0
	scanner := bufio.NewScanner(strings.NewReader(logs.String()))
	for scanner.Scan() {
		var entry struct {
			SQL string `json:"stmt"`
		}
		require.NoError(json.Unmarshal(scanner.Bytes(), &entry))
		if entry.SQL == "" {
			continue
		}
		statements++
		assert.NotRegexp(wholeEvidence, scalarReads.ReplaceAllString(entry.SQL, ""), "summary SQL must avoid fetching JSON evidence for decoding in Go")
	}
	require.NoError(scanner.Err())
	assert.Positive(statements, "the summary query must be captured")
}
