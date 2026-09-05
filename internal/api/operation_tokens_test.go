package api

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/store"
)

func TestOperationTokenRunReferenceIsPrivatePersistentAndArchiveBound(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	st, err := store.OpenForTest(dbPath)
	require.NoError(err)
	require.NoError(st.InitSchema())

	id := mustOperationTextID(t, operations.KindPersonSweep, "private-stable-run-id")
	codec := newOperationTokenCodec(st)
	token, err := codec.encodeRunReference(t.Context(), id, operationTestArchive)
	require.NoError(err)
	assert.True(strings.HasPrefix(token, "op2."))
	for _, private := range []string{
		"{", `"`, operationTestArchive, "private-stable-run-id", "person_sweep", "text",
	} {
		assert.NotContains(token, private)
	}
	require.NoError(st.Close())

	reopened, err := store.OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(reopened.InitSchema())
	decoded, err := newOperationTokenCodec(reopened).decodeRunReference(
		t.Context(), token, operationTestArchive)
	require.NoError(err)
	assert.Equal(id, decoded)

	_, err = newOperationTokenCodec(reopened).decodeRunReference(
		t.Context(), token, "different-archive")
	assert.ErrorIs(err, errInvalidOperationRunReference)
}

func TestOperationTokenRunReferenceRejectsTamperingLegacyAndMalformedInput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := operationTokenTestStore(t)
	codec := newOperationTokenCodec(st)
	id := mustOperationIntID(t, operations.KindSourceSync, 42)
	token, err := codec.encodeRunReference(t.Context(), id, operationTestArchive)
	require.NoError(err)
	parts := strings.Split(token, ".")
	require.Len(parts, 3)

	ciphertext, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(err)
	ciphertext[len(ciphertext)-1] ^= 1
	tampered := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(ciphertext)
	unknownKeyID := strings.Repeat("f", 32)
	if unknownKeyID == parts[1] {
		unknownKeyID = strings.Repeat("e", 32)
	}

	malformed := []string{
		tampered,
		token[:len(token)-1],
		parts[0] + "." + unknownKeyID + "." + parts[2],
		"1." + base64.RawURLEncoding.EncodeToString([]byte(`{"kind":"source_sync","int_id":42}`)),
		"", "op2", "op2..", "op2.invalid." + parts[2], "op2." + parts[1] + ".%%%,",
		"op3." + parts[1] + "." + parts[2], token + ".extra",
	}
	for _, raw := range malformed {
		_, err := codec.decodeRunReference(t.Context(), raw, operationTestArchive)
		require.ErrorIs(err, errInvalidOperationRunReference, raw)
	}

	invalidPayload, err := codec.seal(t.Context(), operationTestArchive,
		map[string]any{"kind": operations.KindSourceSync, "provider": "private"})
	require.NoError(err)
	_, err = codec.decodeRunReference(t.Context(), invalidPayload, operationTestArchive)
	assert.ErrorIs(err, errInvalidOperationRunReference)
}

func TestOperationTokenRunReferenceUsesRetainedKeyUntilExplicitPurge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := operationTokenTestStore(t)
	codec := newOperationTokenCodec(st)
	id := mustOperationIntID(t, operations.KindCardDAVSync, 9)
	token, err := codec.encodeRunReference(t.Context(), id, operationTestArchive)
	require.NoError(err)
	parts := strings.Split(token, ".")
	require.Len(parts, 3)
	retiredKeyID := parts[1]

	_, err = st.RotateOperationTokenKey(t.Context(), time.Date(2026, 8, 30, 15, 0, 0, 0, time.UTC))
	require.NoError(err)
	decoded, err := codec.decodeRunReference(t.Context(), token, operationTestArchive)
	require.NoError(err)
	assert.Equal(id, decoded)

	require.NoError(st.DeleteOperationTokenKey(t.Context(), retiredKeyID))
	_, err = codec.decodeRunReference(t.Context(), token, operationTestArchive)
	assert.ErrorIs(err, errInvalidOperationRunReference)
}

func TestOperationTokenCursorHidesAndRestoresCompleteSnapshotBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := operationTokenTestStore(t)
	codec := newOperationTokenCodec(st)
	startedFrom := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	startedBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	filter := operationHistoryFilter{
		Lane: operations.LanePersonFacts, State: operations.StateFailed,
		StartedFrom: &startedFrom, StartedBefore: &startedBefore,
	}
	binding := operationCursorBinding{
		Position: operations.Position{
			StartedAt: time.Date(2026, 8, 28, 12, 34, 56, 123456000, time.UTC),
			ID:        mustOperationTextID(t, operations.KindPersonSweep, "private-cursor-run-id"),
		},
		MembershipRevision: 73,
		AvailableKinds: []operations.Kind{
			operations.KindPersonEmbedding, operations.KindPersonSweep,
		},
		UnavailableKinds: []operations.Kind{operations.KindPersonEnrichment},
	}
	token, err := codec.encodeCursor(t.Context(), binding, filter, operationTestArchive)
	require.NoError(err)
	for _, private := range []string{
		"{", `"`, operationTestArchive, "private-cursor-run-id", "person_facts", "failed",
		"2026-08-01", "2026-09-01", "person_embedding", "person_enrichment", "person_sweep",
	} {
		assert.NotContains(token, private)
	}

	decoded, err := codec.decodeCursor(t.Context(), token, filter, operationTestArchive)
	require.NoError(err)
	assert.Equal(binding, decoded)

	changed := filter
	changed.StartedBefore = new(startedBefore.Add(24 * time.Hour))
	_, err = codec.decodeCursor(t.Context(), token, changed, operationTestArchive)
	require.ErrorIs(err, errInvalidOperationCursor)

	missingKindSet, err := codec.seal(t.Context(), operationTestArchive, map[string]any{
		"t": binding.Position.StartedAt.Format(time.RFC3339Nano),
		"k": binding.Position.ID.Kind(), "it": binding.Position.ID.Type(),
		"s": "private-cursor-run-id", "f": operationFilterFingerprint(filter),
		"r": binding.MembershipRevision, "ak": binding.AvailableKinds,
	})
	require.NoError(err)
	_, err = codec.decodeCursor(t.Context(), missingKindSet, filter, operationTestArchive)
	assert.ErrorIs(err, errInvalidOperationCursor)
}

func TestOperationCursorRejectsAuthenticatedNoncanonicalTimestamp(t *testing.T) {
	codec := newOperationTokenCodec(operationTokenTestStore(t))
	filter := operationHistoryFilter{}
	intID := int64(42)
	token, err := codec.seal(t.Context(), operationTestArchive, operationCursorPayload{
		Timestamp:          "2026-08-30T00:00:00.000Z",
		Kind:               operations.KindSourceSync,
		IDType:             operations.StableIDInt64,
		IntID:              &intID,
		FilterHash:         operationFilterFingerprint(filter),
		MembershipRevision: 7,
		AvailableKinds:     []operations.Kind{operations.KindSourceSync},
		UnavailableKinds:   []operations.Kind{},
	})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(token, "op2."))

	_, err = codec.decodeCursor(t.Context(), token, filter, operationTestArchive)
	assert.ErrorIs(t, err, errInvalidOperationCursor)
}

func operationTokenTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	return st
}
