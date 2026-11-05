package cmd

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// serial: verify flags, stdout and http.DefaultTransport are process globals.
func TestRunVerifyLocalRawSamples(t *testing.T) {
	resetVerifyFlagsForTest(t)
	verifySampleSize = 1

	// Only Gmail profile responses are simulated. Store reopening,
	// sampling, decompression, MIME parsing and result reporting are real.
	savedTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = savedTransport })
	http.DefaultTransport = testTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != "https://gmail.googleapis.com/gmail/v1/users/me/profile" {
			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
		}
		body := `{"emailAddress":"verify@example.test","messagesTotal":1,"historyId":"100"}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})

	for _, tc := range []struct {
		name         string
		raw          []byte
		wantVerified int
		wantErrors   int
		wantError    string
	}{
		{
			name:         "valid_empty_body",
			raw:          []byte("From: sender@example.test\r\nSubject: Original\r\n\r\n"),
			wantVerified: 1,
		},
		{
			name:       "empty_zlib",
			raw:        []byte{},
			wantErrors: 1,
			wantError:  "empty raw MIME",
		},
		{
			name:       "malformed_mime",
			raw:        []byte("Content-Type: multipart/mixed\r\n\r\nmissing boundary"),
			wantErrors: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			_, invocation := setupCommandOAuth(t)
			putCommandToken(t, invocation, "verify@example.test", oauth.Scopes)
			cfg := invocation.cfg
			var st *store.Store
			testDB := os.Getenv("MSGVAULT_TEST_DB")
			if store.IsPostgresURL(testDB) {
				st = testutil.NewTestStore(t)
				// Point the command at this fixture's clone or private schema,
				// never the shared test database itself.
				dsn, err := url.Parse(testDB)
				require.NoError(err)
				var database, schema string
				require.NoError(st.DB().QueryRow("SELECT current_database(), current_schema()").Scan(&database, &schema))
				dsn.Path = "/" + database
				query := dsn.Query()
				query.Set("search_path", schema+",public")
				dsn.RawQuery = query.Encode()
				cfg.Data.DatabaseURL = dsn.String()
			} else {
				st = testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
			}
			source, err := st.GetOrCreateSource("gmail", "verify@example.test")
			require.NoError(err)
			conversationID, err := st.EnsureConversation(source.ID, "thread", "Original")
			require.NoError(err)
			id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversationID,
				SourceMessageID: "synthetic-verify", MessageType: store.MessageTypeEmail})
			require.NoError(err)
			require.NoError(st.UpsertMessageRaw(id, tc.raw))
			require.NoError(st.Close())
			command := &cobra.Command{Use: "verify"}
			command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			verifyJSON = true
			capture := captureStdout(t)
			err = runVerifyLocal(command, []string{"verify@example.test"})
			output := capture()
			// Sample errors are reported in the summary, as for missing RAW;
			// they do not change the command's existing exit-status contract.
			require.NoError(err)
			var result verifyResult
			require.NoError(json.Unmarshal([]byte(output), &result))
			assert.Equal(tc.wantVerified, result.SampleVerified)
			assert.Equal(tc.wantErrors, result.SampleErrors)
			if tc.wantError != "" {
				verifyJSON = false
				capture = captureStdout(t)
				err = runVerifyLocal(command, []string{"verify@example.test"})
				output = capture()
				require.NoError(err)
				assert.Contains(output, fmt.Sprintf("msg %d: %s", id, tc.wantError))
			}
		})
	}
}

// TestIsFTSIntegrityError_Classification verifies that the hint-classifier
// cleanly separates FTS5 shadow-table errors (which rebuild-fts can fix)
// from core-table errors (which need .recover). Messages come from real
// PRAGMA integrity_check output; the shapes below are what users will see.
func TestIsFTSIntegrityError_Classification(t *testing.T) {
	tests := []struct {
		msg    string
		wantFT bool
	}{
		{
			msg:    "malformed inverted index for FTS5 table main.messages_fts",
			wantFT: true,
		},
		{
			msg:    "row 42 missing from index messages_fts_idx",
			wantFT: true,
		},
		{
			msg:    "Tree 26 page 8231140 cell 2: Rowid 421177 out of order",
			wantFT: false,
		},
		{
			msg:    "non-unique entry in index sqlite_autoindex_messages_1",
			wantFT: false,
		},
		{
			msg:    "",
			wantFT: false,
		},
	}

	for _, tc := range tests {
		got := isFTSIntegrityError(tc.msg)
		assert.Equal(t, tc.wantFT, got, "isFTSIntegrityError(%q)", tc.msg)
	}
}

// TestNewVerifyResult checks the machine-readable summary math behind
// `verify --json`: difference is signed (gmailTotal-archived), raw-MIME
// coverage is a percentage of the archived count that must not divide by zero
// on an empty archive, and integrity/sample state is represented without
// claiming an integrity check ran when it did not.
func TestNewVerifyResult(t *testing.T) {
	integrityOK := true
	tests := []struct {
		name        string
		found       bool
		integrity   *bool
		gmailTotal  int64
		archived    int64
		withRaw     int64
		sampleSize  int
		verified    int
		errors      int
		interrupted bool
		wantDiff    int64
		wantPct     float64
	}{
		{
			name:       "missing from archive",
			found:      true,
			integrity:  &integrityOK,
			gmailTotal: 102415,
			archived:   8508,
			withRaw:    8508,
			sampleSize: 100,
			verified:   100,
			wantDiff:   93907,
			wantPct:    100,
		},
		{
			name:       "empty archive avoids divide by zero",
			found:      true,
			integrity:  &integrityOK,
			gmailTotal: 500,
			archived:   0,
			withRaw:    0,
			wantDiff:   500,
			wantPct:    0,
		},
		{
			name:        "extra in archive is negative",
			found:       true,
			integrity:   &integrityOK,
			gmailTotal:  100,
			archived:    120,
			withRaw:     120,
			sampleSize:  25,
			verified:    24,
			errors:      1,
			interrupted: true,
			wantDiff:    -20,
			wantPct:     100,
		},
		{
			name:       "partial raw coverage",
			found:      true,
			integrity:  nil,
			gmailTotal: 10,
			archived:   10,
			withRaw:    5,
			wantDiff:   0,
			wantPct:    50,
		},
		{
			name:       "account not found in archive",
			found:      false,
			integrity:  &integrityOK,
			gmailTotal: 25,
			archived:   0,
			withRaw:    0,
			wantDiff:   25,
			wantPct:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			got := newVerifyResult(
				"user@example.com",
				tc.found,
				tc.integrity,
				tc.gmailTotal,
				tc.archived,
				tc.withRaw,
				tc.sampleSize,
				tc.verified,
				tc.errors,
				tc.interrupted,
			)
			assert.Equal("user@example.com", got.Email)
			assert.Equal(tc.found, got.ArchiveAccountFound)
			assert.Equal(tc.integrity != nil, got.DatabaseIntegrityChecked)
			assert.Equal(tc.integrity, got.DatabaseIntegrityOK)
			assert.Equal(tc.gmailTotal, got.GmailMessagesTotal)
			assert.Equal(tc.archived, got.ArchivedMessages)
			assert.Equal(tc.withRaw, got.RawMIMEMessages)
			assert.Equal(tc.wantDiff, got.Difference)
			assert.InDelta(tc.wantPct, got.RawMIMECoveragePct, 0.0001)
			assert.Equal(tc.sampleSize, got.SampleSize)
			assert.Equal(tc.verified, got.SampleVerified)
			assert.Equal(tc.errors, got.SampleErrors)
			assert.Equal(tc.interrupted, got.SampleInterrupted)
		})
	}
}
