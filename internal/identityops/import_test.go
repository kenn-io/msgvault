package identityops_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestParseIdentityImportSupportsTextAndStrictJSONShapes(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		format string
		want   []identityops.ImportEntry
	}{
		{
			name:   "text comments blanks and extension",
			input:  "  # exported aliases\nOld@Example.test\n\nactive@example.test\n",
			format: "aliases.txt",
			want: []identityops.ImportEntry{
				{Identifier: "active@example.test"},
				{Identifier: "Old@Example.test"},
			},
		},
		{
			name:   "auto detects string array",
			input:  "  [\"Old@Example.test\",\"active@example.test\"]",
			format: "",
			want: []identityops.ImportEntry{
				{Identifier: "active@example.test"},
				{Identifier: "Old@Example.test"},
			},
		},
		{
			name:   "entry array",
			input:  `[{"identifier":"old@example.test","state":"disabled"}]`,
			format: "json",
			want: []identityops.ImportEntry{
				{Identifier: "old@example.test", State: "disabled"},
			},
		},
		{
			name:   "object",
			input:  `{"identities":[{"identifier":"waiting@example.test","state":"pending"}]}`,
			format: ".json",
			want: []identityops.ImportEntry{
				{Identifier: "waiting@example.test", State: "pending"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := identityops.ParseImport(strings.NewReader(test.input), test.format)

			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestIdentityImportPreviewAndApplyShareDeterministicCandidates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	entries := []identityops.ImportEntry{
		{Identifier: "Waiting@Example.test", State: "pending"},
		{Identifier: "known@example.test", State: "deleted"},
		{Identifier: "alias@example.test", State: "enabled"},
	}
	previewStore := newDiscoveryFakeStore()
	previewStore.identities = []store.AccountIdentity{{
		SourceID: 14, Address: "Known@Example.test", SourceSignal: "manual",
	}}

	preview, err := identityops.Import(t.Context(), previewStore, identityops.ImportRequest{
		SourceID: 14,
		Entries:  entries,
	})
	requirements.NoError(err)
	assertions.Empty(previewStore.batchCalls)
	assertions.Equal("manual", preview.Signal)
	encoded, err := json.Marshal(preview.Candidates)
	requirements.NoError(err)
	assertions.JSONEq(`[
		{"identifier":"alias@example.test","normalized_identifier":"alias@example.test","classification":"strong","already_confirmed":false,"signals":["manual"],"provider_states":["enabled"],"sent_message_count":0,"received_message_count":0,"first_seen_at":"0001-01-01T00:00:00Z","last_seen_at":"0001-01-01T00:00:00Z"},
		{"identifier":"Known@Example.test","normalized_identifier":"known@example.test","classification":"confirmed","already_confirmed":true,"signals":["manual"],"provider_states":["deleted"],"sent_message_count":0,"received_message_count":0,"first_seen_at":"0001-01-01T00:00:00Z","last_seen_at":"0001-01-01T00:00:00Z"},
		{"identifier":"Waiting@Example.test","normalized_identifier":"waiting@example.test","classification":"strong","already_confirmed":false,"signals":["manual"],"provider_states":["pending"],"sent_message_count":0,"received_message_count":0,"first_seen_at":"0001-01-01T00:00:00Z","last_seen_at":"0001-01-01T00:00:00Z"}
	]`, string(encoded))

	applyStore := newDiscoveryFakeStore()
	applyStore.identities = previewStore.identities
	apply, err := identityops.Import(t.Context(), applyStore, identityops.ImportRequest{
		SourceID: 14,
		Entries:  entries,
		Apply:    true,
	})
	requirements.NoError(err)
	assertions.Equal(preview.Candidates, apply.Candidates)
	requirements.Len(applyStore.batchCalls, 1)
	assertions.Equal([]store.IdentityConfirmation{
		{Identifier: "alias@example.test", Signals: []string{"manual"}},
		{Identifier: "Waiting@Example.test", Signals: []string{"manual"}},
	}, applyStore.batchCalls[0], "pending and historical states are report-only")
}

func TestIdentityImportValidatesEveryRowAndSignalBeforeWriting(t *testing.T) {
	tests := []struct {
		name    string
		entries []identityops.ImportEntry
		signal  string
		want    string
	}{
		{
			name: "invalid later row",
			entries: []identityops.ImportEntry{
				{Identifier: "good@example.test"},
				{Identifier: "not an address"},
			},
			want: "concrete mailbox address",
		},
		{
			name:    "conflicting duplicate state",
			entries: []identityops.ImportEntry{{Identifier: "Alias@example.test", State: "enabled"}, {Identifier: "alias@example.test", State: "deleted"}},
			want:    "conflicting states",
		},
		{
			name:    "comma signal",
			entries: []identityops.ImportEntry{{Identifier: "good@example.test"}},
			signal:  "manual,provider",
			want:    "cannot contain commas",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := newDiscoveryFakeStore()
			_, err := identityops.Import(t.Context(), st, identityops.ImportRequest{
				SourceID: 14,
				Entries:  test.entries,
				Signal:   test.signal,
				Apply:    true,
			})

			require.ErrorContains(t, err, test.want)
			assert.Empty(t, st.batchCalls)
		})
	}
}

func TestIdentityImportApplyIsSourceScopedStateIndependentAndIdempotent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "primary@example.test")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("imap", "other@example.test")
	requirements.NoError(err)
	requirements.NoError(st.AddAccountIdentity(source.ID, "Old@Example.test", "manual"))
	requirements.NoError(st.AddAccountIdentity(other.ID, "other-alias@example.test", "manual"))
	req := identityops.ImportRequest{
		SourceID: source.ID,
		Entries: []identityops.ImportEntry{
			{Identifier: "old@example.test", State: "deleted"},
			{Identifier: "waiting@example.test", State: "pending"},
		},
		Apply: true,
	}

	first, err := identityops.Import(t.Context(), st, req)
	requirements.NoError(err)
	assertions.Equal([]store.IdentityConfirmationOutcome{{
		Identifier: "waiting@example.test", Added: true, Signals: []string{"manual"},
	}}, first.Applied)
	retry, err := identityops.Import(t.Context(), st, req)
	requirements.NoError(err)
	assertions.Empty(retry.Applied)

	identities, err := st.ListAccountIdentities(source.ID)
	requirements.NoError(err)
	assertions.Equal([]string{"Old@Example.test", "waiting@example.test"}, accountIdentityAddresses(identities))
	otherIdentities, err := st.ListAccountIdentities(other.ID)
	requirements.NoError(err)
	assertions.Equal([]string{"other-alias@example.test"}, accountIdentityAddresses(otherIdentities))
}

func TestIdentityImportMergesAdditionalSignalIdempotently(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "primary@example.test")
	requirements.NoError(err)
	requirements.NoError(st.AddAccountIdentity(source.ID, "Alias@Example.test", "manual"))
	req := identityops.ImportRequest{
		SourceID: source.ID,
		Entries:  []identityops.ImportEntry{{Identifier: "alias@example.test", State: "deleted"}},
		Signal:   "bulk-import",
		Apply:    true,
	}

	first, err := identityops.Import(t.Context(), st, req)
	requirements.NoError(err)
	assertions.Equal([]store.IdentityConfirmationOutcome{{
		Identifier: "Alias@Example.test", Added: false, Signals: []string{"bulk-import"},
	}}, first.Applied)
	identities, err := st.ListAccountIdentities(source.ID)
	requirements.NoError(err)
	requirements.Len(identities, 1)
	assertions.Equal("Alias@Example.test", identities[0].Address)
	assertions.Equal("bulk-import,manual", identities[0].SourceSignal)

	retry, err := identityops.Import(t.Context(), st, req)
	requirements.NoError(err)
	assertions.Empty(retry.Applied)
}

func TestIdentityImportCancellationPreventsWrites(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	st := newDiscoveryFakeStore()

	_, err := identityops.Import(ctx, st, identityops.ImportRequest{
		SourceID: 14,
		Entries:  []identityops.ImportEntry{{Identifier: "alias@example.test"}},
		Apply:    true,
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, st.batchCalls)
}

func TestIdentityImportReturnsCommittedPrefixOnBatchError(t *testing.T) {
	st := newDiscoveryFakeStore()
	commitErr := errors.New("second chunk failed")
	st.batchFunc = func(_ context.Context, confirmations []store.IdentityConfirmation) ([]store.IdentityConfirmationOutcome, error) {
		return []store.IdentityConfirmationOutcome{{
			Identifier: confirmations[0].Identifier,
			Added:      true,
			Signals:    confirmations[0].Signals,
		}}, commitErr
	}

	result, err := identityops.Import(t.Context(), st, identityops.ImportRequest{
		SourceID: 14,
		Entries: []identityops.ImportEntry{
			{Identifier: "first@example.test"},
			{Identifier: "second@example.test"},
		},
		Apply: true,
	})

	require.ErrorIs(t, err, commitErr)
	assert.Equal(t, []store.IdentityConfirmationOutcome{{
		Identifier: "first@example.test", Added: true, Signals: []string{"manual"},
	}}, result.Applied)
}

func TestIdentityImportJSONTracksExplicitNullSourceID(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var req identityops.ImportRequest
	requirements.NoError(json.Unmarshal([]byte(`{
		"source_id":null,
		"entries":[{"identifier":"alias@example.test"}]
	}`), &req))

	st := newDiscoveryFakeStore()
	_, err := identityops.Import(t.Context(), st, req)

	requirements.Error(err)
	requirements.ErrorContains(err, "source ID must be positive")
	assertions.Empty(st.batchCalls)
}

func TestParseIdentityImportPreservesFirstSpellingAndRejectsConflictingStates(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	got, err := identityops.ParseImport(strings.NewReader(`[
		{"identifier":"Alias@Example.test","state":"disabled"},
		{"identifier":"alias@example.test","state":"disabled"},
		{"identifier":"zeta@example.test"}
	]`), "json")

	requirements.NoError(err)
	assertions.Equal([]identityops.ImportEntry{
		{Identifier: "Alias@Example.test", State: "disabled"},
		{Identifier: "zeta@example.test"},
	}, got)

	_, err = identityops.ParseImport(strings.NewReader(`[
		{"identifier":"Alias@Example.test","state":"disabled"},
		{"identifier":"alias@example.test","state":"enabled"}
	]`), "json")
	requirements.Error(err)
	assertions.ErrorContains(err, "conflicting states")
}

func TestParseIdentityImportRejectsMalformedOrUnsafeInput(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		format string
		want   string
	}{
		{name: "unknown format", input: "alias@example.test", format: "csv", want: "unsupported identity import format"},
		{name: "empty", input: " # only a comment\n", format: "text", want: "no identities"},
		{name: "invalid text address", input: "not an address\n", format: "text", want: "concrete mailbox address"},
		{name: "wildcard", input: "*@example.test\n", format: "text", want: "concrete mailbox address"},
		{name: "unknown envelope field", input: `{"identities":[],"extra":true}`, format: "json", want: "unknown field"},
		{name: "unknown entry field", input: `[{"identifier":"alias@example.test","extra":true}]`, format: "json", want: "unknown field"},
		{name: "trailing document", input: `["alias@example.test"] {}`, format: "json", want: "single JSON document"},
		{name: "mixed array", input: `["alias@example.test",{"identifier":"other@example.test"}]`, format: "json", want: "array of strings or identity entries"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := identityops.ParseImport(strings.NewReader(test.input), test.format)

			require.Error(t, err)
			assert.ErrorContains(t, err, test.want)
		})
	}
}
