package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
