package store_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func FuzzDocumentFailureDetail(f *testing.F) {
	for _, value := range []string{"PDF end marker is missing or not final", "first line\nprivate second line", "\x1b[31mred\x1b[0m", strings.Repeat("é", 1024), "\xff\x00bad"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		detail := store.CleanDocumentFailureDetail(value)
		assert.True(t, utf8.ValidString(detail))
		assert.LessOrEqual(t, len(detail), 1024)
		assert.NotContains(t, detail, "\n")
		for _, char := range detail {
			require.False(t, unicode.IsControl(char) || unicode.In(char, unicode.Cf, unicode.Zl, unicode.Zp))
		}
		assert.Equal(t, detail, store.CleanDocumentFailureDetail(detail))
	})
}

func TestDocumentFailureDiagnosticsPersistAcrossRetryAndRebuild(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	input := documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "failure-first", ProfileID: profile.ID, CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "failure-worker", LeaseUntil: time.Now().Add(time.Hour), LocalBytes: 128, SourceSequence: 1,
	})
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: claim, ReasonCode: "invalid_local_source", Detail: "PDF end marker is missing or not final\nprivate remainder", Terminal: true,
	}))
	status, err := f.Store.GetDocumentIndexStatusForScope(t.Context(), profile.ID, "original", profile.AllowedMediaTypes, nil)
	require.NoError(err)
	require.Len(status.Failures, 1)
	assert.Equal("PDF end marker is missing or not final", status.Failures[0].Detail)
	assert.Equal(hash, status.Failures[0].CanonicalBlobHash)
	changed, err := f.Store.RetryDocumentExtraction(t.Context(), profile.ID, hash)
	require.NoError(err)
	require.True(changed)
	input.ExtractionID = "failure-second"
	claim, err = f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: claim, ReasonCode: "spool_capacity_unavailable", Detail: "spool quota is exhausted", RetryAt: time.Now().Add(time.Minute),
	}))
	// Retry touches the old row too; diagnostic ordering follows creation, not retry timestamps.
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_extractions SET created_at = ?, updated_at = ? WHERE id = ?"), time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "failure-first")
	require.NoError(err)
	status, err = f.Store.GetDocumentIndexStatusForScope(t.Context(), profile.ID, "original", profile.AllowedMediaTypes, nil)
	require.NoError(err)
	require.Len(status.Failures, 1)
	assert.Equal("spool quota is exhausted", status.Failures[0].Detail)
	changed, err = f.Store.RetryDocumentExtraction(t.Context(), profile.ID, hash)
	require.NoError(err)
	require.True(changed)
	input.ExtractionID = "failure-success"
	claim, err = f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(err)
	require.NoError(f.Store.PublishDocumentExtraction(t.Context(), publicationFor(t, claim, "quasar evidence", strings.Repeat("d", 64))))
	status, err = f.Store.GetDocumentIndexStatusForScope(t.Context(), profile.ID, "original", profile.AllowedMediaTypes, nil)
	require.NoError(err)
	assert.Empty(status.Failures)
	rebuild, err := f.Store.StartDocumentExtractionRebuild(t.Context(), "failed-replacement", profile.ID, "original", profile.AllowedMediaTypes, nil)
	require.NoError(err)
	input.RebuildID = rebuild.ID
	input.ExtractionID = "failure-rebuild"
	claim, err = f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{Claim: claim, ReasonCode: "provider_transient", RetryAt: time.Now().Add(time.Minute)}))
	status, err = f.Store.GetDocumentIndexStatusForScope(t.Context(), profile.ID, "original", profile.AllowedMediaTypes, nil)
	require.NoError(err)
	require.Len(status.Failures, 1)
	assert.Equal("provider_transient", status.Failures[0].ReasonCode)
	// All legacy attempts may share a second-resolution timestamp. New attempts
	// must preserve chronological order even when caller IDs sort backwards.
	var successCreated, failureCreated time.Time
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT created_at FROM document_extractions WHERE id = ?"), "failure-success").Scan(&successCreated))
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT created_at FROM document_extractions WHERE id = ?"), "failure-rebuild").Scan(&failureCreated))
	assert.True(failureCreated.After(successCreated), "attempt chronology must not rely on lexical extraction IDs")
	// A successful replacement hides all historical failures.
	changed, err = f.Store.RetryDocumentExtraction(t.Context(), profile.ID, hash)
	require.NoError(err)
	require.True(changed)
	input.ExtractionID = "replacement-success"
	claim, err = f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(err)
	require.NoError(f.Store.PublishDocumentExtraction(t.Context(), publicationFor(t, claim, "nebula replacement", strings.Repeat("e", 64))))
	status, err = f.Store.GetDocumentIndexStatusForScope(t.Context(), profile.ID, "original", profile.AllowedMediaTypes, nil)
	require.NoError(err)
	assert.Empty(status.Failures)
}

func TestDocumentFailureDiagnosticsFollowNewestAttemptWhenClockRepeats(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	input := documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "z-earlier", ProfileID: profile.ID, CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "clock-worker", LeaseUntil: time.Now().Add(time.Hour), LocalBytes: 128, SourceSequence: 1,
	})
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: claim, ReasonCode: "invalid_local_source", Detail: "earlier failure", Terminal: true,
	}))
	// A coarse or stepped-back clock: the earlier attempt's timestamp is not
	// before the time the next claim reads, and its ID sorts after the next one.
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_extractions SET created_at = ? WHERE id = ?"),
		time.Now().Add(time.Hour).UTC(), "z-earlier")
	require.NoError(err)
	changed, err := f.Store.RetryDocumentExtraction(t.Context(), profile.ID, hash)
	require.NoError(err)
	require.True(changed)
	input.ExtractionID = "a-later"
	claim, err = f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: claim, ReasonCode: "provider_transient", Detail: "later failure", Terminal: true,
	}))
	status, err := f.Store.GetDocumentIndexStatusForScope(t.Context(), profile.ID, "original", profile.AllowedMediaTypes, nil)
	require.NoError(err)
	require.Len(status.Failures, 1)
	assert.Equal("provider_transient", status.Failures[0].ReasonCode, "diagnostics report the newest attempt")
	assert.Equal("later failure", status.Failures[0].Detail)
}

func TestDocumentFailureDiagnosticsBoundAndLegacyFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	profile, _ := seedDocumentPublicationAuthority(t, f)
	for i := range 21 {
		hash := fmt.Sprintf("%064x", i+10)
		message := f.CreateMessage(fmt.Sprintf("failed-document-%d", i))
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), message, store.AttachmentWrite{Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: 128, StoragePath: hash[:2] + "/" + hash, ContentHash: hash, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}))
		_, _, err := f.Store.ReconcileDocumentOccurrence(t.Context(), singleAttachmentID(t, f, message), 1)
		require.NoError(err)
		// Legacy records have only terminal_reason.
		_, err = f.Store.DB().Exec(f.Store.Rebind("INSERT INTO document_extractions (id,profile_id,canonical_blob_hash,extraction_input_key,state,local_bytes,terminal_reason) VALUES (?,?,?,'original','terminal',128,'invalid_local_source')"), fmt.Sprintf("legacy-failure-%d", i), profile.ID, hash)
		require.NoError(err)
	}
	status, err := f.Store.GetDocumentIndexStatusForScope(t.Context(), profile.ID, "original", profile.AllowedMediaTypes, nil)
	require.NoError(err)
	require.Len(status.Failures, 20)
	assert.False(status.FailuresExhausted)
	assert.Equal("invalid_local_source", status.Failures[0].ReasonCode)
	assert.Empty(status.Failures[0].Detail)
}

func TestDocumentFailureSurvivesStoreReopen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// Exercise persistence on a file-backed SQLite archive, independently of
	// the configured backend used by the lifecycle tests above.
	t.Setenv("MSGVAULT_TEST_DB", "")
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	input := documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "persisted-failure", ProfileID: profile.ID, CanonicalBlobHash: hash,
		ExtractionInputKey: "original", LeaseOwner: "restart-worker", LeaseUntil: time.Now().Add(time.Hour), LocalBytes: 128, SourceSequence: 1,
	})
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: claim, ReasonCode: "invalid_local_source", Detail: "PDF end marker is missing or not final", Terminal: true,
	}))
	var sequence int
	var name, path string
	require.NoError(f.Store.DB().QueryRow("PRAGMA database_list").Scan(&sequence, &name, &path))
	require.NoError(f.Store.Close())
	reopened, err := store.Open(path)
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	status, err := reopened.GetDocumentIndexStatusForScope(t.Context(), profile.ID, "original", profile.AllowedMediaTypes, nil)
	require.NoError(err)
	require.Len(status.Failures, 1)
	assert.Equal("PDF end marker is missing or not final", status.Failures[0].Detail)
	assert.Equal("invalid_local_source", status.Failures[0].ReasonCode)
}
