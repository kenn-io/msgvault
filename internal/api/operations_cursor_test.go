package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
)

const (
	operationTestArchive = "archive-fixture-01"
	operationTestHash    = "09400aa4c775f0beee51af164d411202bd86e7e50263e7b8109317a77c1e6c99"
)

func TestOperationRunReferenceRoundTripsTypedIDs(t *testing.T) {
	tests := []struct {
		name        string
		id          operations.StableID
		wantPayload string
	}{
		{
			name:        "source numeric",
			id:          mustOperationIntID(t, operations.KindSourceSync, 42),
			wantPayload: `{"kind":"source_sync","id_type":"int64","int_id":42,"archive_uid":"archive-fixture-01"}`,
		},
		{
			name:        "CardDAV numeric",
			id:          mustOperationIntID(t, operations.KindCardDAVSync, 9),
			wantPayload: `{"kind":"carddav_sync","id_type":"int64","int_id":9,"archive_uid":"archive-fixture-01"}`,
		},
		{
			name:        "person sweep text",
			id:          mustOperationTextID(t, operations.KindPersonSweep, "sweep-fixture-01"),
			wantPayload: `{"kind":"person_sweep","id_type":"text","string_id":"sweep-fixture-01","archive_uid":"archive-fixture-01"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			token, err := encodeOperationRunReference(test.id, operationTestArchive)
			require.NoError(err)
			assert.Equal(test.wantPayload, operationTokenPayload(t, token))

			decoded, err := decodeOperationRunReference(token, operationTestArchive)
			require.NoError(err)
			assert.Equal(test.id, decoded)
		})
	}
}

func TestOperationRunReferenceRejectsMalformedOrNoncanonicalTokens(t *testing.T) {
	oversized := strings.Repeat("x", operations.MaxTextStableIDBytes+1)
	valid := `{"kind":"source_sync","id_type":"int64","int_id":1,"archive_uid":"archive-fixture-01"}`
	tests := []struct {
		name string
		raw  string
	}{
		{"missing version", operationRawToken(valid)},
		{"malformed version", "x." + operationRawToken(`{}`)},
		{"future version", "2." + operationRawToken(`{}`)},
		{"missing payload", "1."},
		{"invalid raw base64", "1.%%%"},
		{"padded base64", "1.e30="},
		{"invalid JSON", operationVersionedToken(`{"kind":`)},
		{"mixed case kind alias", operationVersionedToken(strings.Replace(valid, `"kind"`, `"Kind"`, 1))},
		{"mixed case ID type alias", operationVersionedToken(strings.Replace(valid, `"id_type"`, `"ID_TYPE"`, 1))},
		{"semantic duplicate field", operationVersionedToken(strings.Replace(valid, `"kind":"source_sync"`, `"kind":"carddav_sync","Kind":"source_sync"`, 1))},
		{"invalid UTF-8 string", operationInvalidUTF8RunToken()},
		{"unknown field", operationVersionedToken(strings.TrimSuffix(valid, "}") + `,"provider":"private"}`)},
		{"duplicate field", operationVersionedToken(strings.Replace(valid, `"kind":"source_sync"`, `"kind":"source_sync","kind":"carddav_sync"`, 1))},
		{"trailing JSON", operationVersionedToken(valid + `{}`)},
		{"null object", operationVersionedToken(`null`)},
		{"null required field", operationVersionedToken(strings.Replace(valid, `"kind":"source_sync"`, `"kind":null`, 1))},
		{"missing kind", operationVersionedToken(strings.Replace(valid, `"kind":"source_sync",`, "", 1))},
		{"unknown kind", operationVersionedToken(strings.Replace(valid, "source_sync", "provider_sync", 1))},
		{"unknown ID type", operationVersionedToken(strings.Replace(valid, `"id_type":"int64"`, `"id_type":"number"`, 1))},
		{"wrong numeric kind pair", operationVersionedToken(strings.Replace(valid, "source_sync", "person_sweep", 1))},
		{"wrong text kind pair", operationVersionedToken(`{"kind":"source_sync","id_type":"text","string_id":"run-1","archive_uid":"archive-fixture-01"}`)},
		{"both ID variants", operationVersionedToken(strings.Replace(valid, `"int_id":1`, `"int_id":1,"string_id":"run-1"`, 1))},
		{"null alternate ID variant", operationVersionedToken(strings.Replace(valid, `"int_id":1`, `"int_id":1,"string_id":null`, 1))},
		{"neither ID variant", operationVersionedToken(strings.Replace(valid, `,"int_id":1`, "", 1))},
		{"null numeric ID", operationVersionedToken(strings.Replace(valid, `"int_id":1`, `"int_id":null`, 1))},
		{"zero numeric ID", operationVersionedToken(strings.Replace(valid, `"int_id":1`, `"int_id":0`, 1))},
		{"negative numeric ID", operationVersionedToken(strings.Replace(valid, `"int_id":1`, `"int_id":-1`, 1))},
		{"empty text ID", operationVersionedToken(`{"kind":"person_sweep","id_type":"text","string_id":"","archive_uid":"archive-fixture-01"}`)},
		{"oversized text ID", operationVersionedToken(`{"kind":"person_sweep","id_type":"text","string_id":"` + oversized + `","archive_uid":"archive-fixture-01"}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeOperationRunReference(test.raw, operationTestArchive)
			assert.ErrorIs(t, err, errInvalidOperationRunReference)
		})
	}
}

func TestOperationRunReferenceBindsArchiveAndValidatesArchiveUID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	id := mustOperationIntID(t, operations.KindSourceSync, 42)
	token, err := encodeOperationRunReference(id, operationTestArchive)
	require.NoError(err)

	decoded, err := decodeOperationRunReference(token, operationTestArchive)
	require.NoError(err)
	assert.Equal(id, decoded)

	_, err = decodeOperationRunReference(token, "another-archive")
	require.ErrorIs(err, errInvalidOperationRunReference)
	_, err = encodeOperationRunReference(id, "")
	require.Error(err)
	_, err = encodeOperationRunReference(id, strings.Repeat("a", maxOperationArchiveUIDBytes+1))
	require.Error(err)

	for _, payload := range []string{
		`{"kind":"source_sync","id_type":"int64","int_id":42,"archive_uid":null}`,
		`{"kind":"source_sync","id_type":"int64","int_id":42,"archive_uid":""}`,
		`{"kind":"source_sync","id_type":"int64","int_id":42,"archive_uid":"` + strings.Repeat("a", maxOperationArchiveUIDBytes+1) + `"}`,
	} {
		_, err = decodeOperationRunReference(operationVersionedToken(payload), operationTestArchive)
		require.ErrorIs(err, errInvalidOperationRunReference)
	}
}

func TestOperationCursorRoundTripsAndBindsArchiveAndFilter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	filter := operationHistoryFilter{
		Kind: operations.KindSourceSync, Lane: operations.LaneMessages, State: operations.StateFailed,
	}
	position := operations.Position{
		StartedAt: time.Date(2026, 8, 28, 12, 34, 56, 123456000, time.UTC),
		ID:        mustOperationIntID(t, operations.KindSourceSync, 42),
	}
	token, err := encodeOperationCursor(position, filter, operationTestArchive)
	require.NoError(err)
	assert.JSONEq(
		`{"t":"2026-08-28T12:34:56.123456Z","k":"source_sync","it":"int64","i":42,"f":"`+
			operationTestHash+`","a":"archive-fixture-01"}`,
		operationTokenPayload(t, token))

	decoded, err := decodeOperationCursor(token, filter, operationTestArchive)
	require.NoError(err)
	assert.Equal(position, decoded)

	_, err = decodeOperationCursor(token, filter, "another-archive")
	require.ErrorIs(err, errInvalidOperationCursor)
	for _, changed := range []operationHistoryFilter{
		{Lane: operations.LaneMessages, State: operations.StateFailed},
		{Kind: operations.KindSourceSync, State: operations.StateFailed},
		{Kind: operations.KindSourceSync, Lane: operations.LaneMessages},
	} {
		_, err = decodeOperationCursor(token, changed, operationTestArchive)
		require.ErrorIs(err, errInvalidOperationCursor)
	}
}

func TestOperationCursorRejectsMalformedOrNoncanonicalTokens(t *testing.T) {
	filter := operationHistoryFilter{
		Kind: operations.KindSourceSync, Lane: operations.LaneMessages, State: operations.StateFailed,
	}
	valid := `{"t":"2026-08-28T12:34:56.123456Z","k":"source_sync","it":"int64","i":42,"f":"` +
		operationTestHash + `","a":"archive-fixture-01"}`
	tests := []struct {
		name string
		raw  string
	}{
		{"missing version", operationRawToken(valid)},
		{"malformed version", "v1." + operationRawToken(valid)},
		{"future version", "2." + operationRawToken(valid)},
		{"missing payload", "1."},
		{"invalid raw base64", "1.%%%"},
		{"invalid JSON", operationVersionedToken(`{"t":`)},
		{"mixed case kind alias", operationVersionedToken(strings.Replace(valid, `"k":"source_sync"`, `"K":"source_sync"`, 1))},
		{"semantic duplicate field", operationVersionedToken(strings.Replace(valid, `"k":"source_sync"`, `"k":"carddav_sync","K":"source_sync"`, 1))},
		{"unknown field", operationVersionedToken(strings.TrimSuffix(valid, "}") + `,"provider_cursor":"private"}`)},
		{"duplicate field", operationVersionedToken(strings.Replace(valid, `"k":"source_sync"`, `"k":"source_sync","k":"carddav_sync"`, 1))},
		{"trailing JSON", operationVersionedToken(valid + `{}`)},
		{"null object", operationVersionedToken(`null`)},
		{"null required field", operationVersionedToken(strings.Replace(valid, `"t":"2026-08-28T12:34:56.123456Z"`, `"t":null`, 1))},
		{"missing timestamp", operationVersionedToken(strings.Replace(valid, `"t":"2026-08-28T12:34:56.123456Z",`, "", 1))},
		{"malformed timestamp", operationVersionedToken(strings.Replace(valid, "2026-08-28T12:34:56.123456Z", "yesterday", 1))},
		{"non UTC timestamp", operationVersionedToken(strings.Replace(valid, "2026-08-28T12:34:56.123456Z", "2026-08-28T14:34:56.123456+02:00", 1))},
		{"unknown kind", operationVersionedToken(strings.Replace(valid, "source_sync", "provider_sync", 1))},
		{"unknown ID type", operationVersionedToken(strings.Replace(valid, `"it":"int64"`, `"it":"number"`, 1))},
		{"wrong kind ID pair", operationVersionedToken(strings.Replace(valid, "source_sync", "person_sweep", 1))},
		{"both ID variants", operationVersionedToken(strings.Replace(valid, `"i":42`, `"i":42,"s":"run-1"`, 1))},
		{"null alternate ID variant", operationVersionedToken(strings.Replace(valid, `"i":42`, `"i":42,"s":null`, 1))},
		{"neither ID variant", operationVersionedToken(strings.Replace(valid, `,"i":42`, "", 1))},
		{"nonpositive ID", operationVersionedToken(strings.Replace(valid, `"i":42`, `"i":0`, 1))},
		{"invalid filter hash", operationVersionedToken(strings.Replace(valid, operationTestHash, "ABC", 1))},
		{"missing filter hash", operationVersionedToken(strings.Replace(valid, `,"f":"`+operationTestHash+`"`, "", 1))},
		{"empty archive", operationVersionedToken(strings.Replace(valid, operationTestArchive, "", 1))},
		{"missing archive", operationVersionedToken(strings.Replace(valid, `,"a":"archive-fixture-01"`, "", 1))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeOperationCursor(test.raw, filter, operationTestArchive)
			assert.ErrorIs(t, err, errInvalidOperationCursor)
		})
	}
}

func TestOperationCursorSupportsTextStableID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	position := operations.Position{
		StartedAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
		ID:        mustOperationTextID(t, operations.KindPersonSweep, "sweep-fixture-02"),
	}
	filter := operationHistoryFilter{Kind: operations.KindPersonSweep}
	token, err := encodeOperationCursor(position, filter, operationTestArchive)
	require.NoError(err)
	assert.Contains(operationTokenPayload(t, token), `"it":"text","s":"sweep-fixture-02"`)
	decoded, err := decodeOperationCursor(token, filter, operationTestArchive)
	require.NoError(err)
	assert.Equal(position, decoded)
}

func TestOperationTokenEnvelopeRequiresCanonicalRawBase64(t *testing.T) {
	newAssertions := assert.New
	require := require.New(t)
	assert := assert.New(t)
	id := mustOperationIntID(t, operations.KindSourceSync, 42)
	canonical, err := encodeOperationRunReference(id, operationTestArchive)
	require.NoError(err)
	prefix, encoded, found := strings.Cut(canonical, ".")
	require.True(found)
	alternateToken, err := encodeOperationRunReference(id, operationTestArchive+"x")
	require.NoError(err)
	_, alternateEncoded, found := strings.Cut(alternateToken, ".")
	require.True(found)

	tests := []struct {
		name string
		raw  string
	}{
		{"embedded newline", prefix + "." + encoded[:4] + "\n" + encoded[4:]},
		{"alternate tail bits", prefix + "." + operationAlternateBase64Tail(t, alternateEncoded)},
		{"oversized envelope", prefix + "." + strings.Repeat("A", base64.RawURLEncoding.EncodedLen(maxOperationTokenPayloadBytes)+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := newAssertions(t)
			_, err := decodeOperationRunReference(test.raw, operationTestArchive)
			assert.ErrorIs(err, errInvalidOperationRunReference)
		})
	}

	atLimit := bytes.Repeat([]byte{'x'}, maxOperationTokenPayloadBytes)
	encodedAtLimit := base64.RawURLEncoding.EncodeToString(atLimit)
	decoded, err := decodeOperationToken(operationTokenVersion + "." + encodedAtLimit)
	require.NoError(err)
	assert.Equal(atLimit, decoded)
	tooLarge := append(bytes.Clone(atLimit), 'x')
	_, err = decodeOperationToken(operationTokenVersion + "." + base64.RawURLEncoding.EncodeToString(tooLarge))
	assert.Error(err)
}

func TestOperationCursorRejectsOversizedArchiveUID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	position := operations.Position{
		StartedAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
		ID:        mustOperationIntID(t, operations.KindSourceSync, 42),
	}
	filter := operationHistoryFilter{Kind: operations.KindSourceSync}
	atLimit := strings.Repeat("a", maxOperationArchiveUIDBytes)
	token, err := encodeOperationCursor(position, filter, atLimit)
	require.NoError(err)
	decoded, err := decodeOperationCursor(token, filter, atLimit)
	require.NoError(err)
	assert.Equal(position, decoded)

	_, err = encodeOperationCursor(position, filter,
		strings.Repeat("a", maxOperationArchiveUIDBytes+1))
	assert.Error(err)
}

func TestOperationQueryParsesDefaultsAndNormalizedFilters(t *testing.T) {
	tests := []struct {
		name      string
		rawQuery  string
		wantKinds []operations.Kind
		wantState []operations.State
		wantLimit int
	}{
		{name: "defaults", wantLimit: 25},
		{name: "kind", rawQuery: "kind=source_sync", wantKinds: []operations.Kind{operations.KindSourceSync}, wantLimit: 25},
		{name: "lane", rawQuery: "lane=messages", wantKinds: []operations.Kind{operations.KindSourceSync}, wantLimit: 25},
		{name: "mixed people lane", rawQuery: "lane=person_facts", wantKinds: []operations.Kind{operations.KindPersonSweep}, wantLimit: 25},
		{name: "unavailable-only lane", rawQuery: "lane=documents", wantKinds: []operations.Kind{}, wantLimit: 25},
		{name: "matching kind and lane", rawQuery: "kind=carddav_sync&lane=contacts", wantKinds: []operations.Kind{operations.KindCardDAVSync}, wantLimit: 25},
		{name: "state and limit", rawQuery: "state=partial&limit=100", wantState: []operations.State{operations.StatePartial}, wantLimit: 100},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			request := httptest.NewRequest(http.MethodGet, "/api/v1/operations/runs?"+test.rawQuery, nil)
			parsed, err := parseOperationRunsQuery(request, operationTestArchive)
			require.NoError(t, err)
			assert.Equal(test.wantKinds, parsed.Query.Kinds)
			assert.Equal(test.wantState, parsed.Query.States)
			assert.Equal(test.wantLimit, parsed.Query.Limit)
			assert.Nil(parsed.Query.Position)
			assert.NoError(parsed.Query.Validate())
		})
	}
}

func TestOperationQueryRejectsDuplicateUnknownAndOutOfRangeParameters(t *testing.T) {
	tests := []struct {
		name     string
		rawQuery string
	}{
		{"duplicate kind", "kind=source_sync&kind=source_sync"},
		{"duplicate lane", "lane=messages&lane=messages"},
		{"duplicate state", "state=failed&state=failed"},
		{"duplicate limit", "limit=25&limit=25"},
		{"multiple cursors", "cursor=first&cursor=second"},
		{"unknown parameter", "provider=gmail"},
		{"unknown kind", "kind=provider_sync"},
		{"unknown lane", "lane=provider"},
		{"unknown state", "state=complete"},
		{"incompatible kind and lane", "kind=source_sync&lane=contacts"},
		{"empty kind", "kind="},
		{"empty cursor", "cursor="},
		{"zero limit", "limit=0"},
		{"limit above maximum", "limit=101"},
		{"negative limit", "limit=-1"},
		{"nonnumeric limit", "limit=twenty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/v1/operations/runs?"+test.rawQuery, nil)
			_, err := parseOperationRunsQuery(request, operationTestArchive)
			assert.Error(t, err)
		})
	}
}

func TestOperationQueryCursorExcludesLimitAndBindsSemanticFilters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	first := httptest.NewRequest(http.MethodGet,
		"/api/v1/operations/runs?state=failed&limit=10&lane=messages&kind=source_sync", nil)
	parsedFirst, err := parseOperationRunsQuery(first, operationTestArchive)
	require.NoError(err)
	assert.Equal(operationTestHash, operationFilterFingerprint(parsedFirst.filter))

	position := operations.Position{
		StartedAt: time.Date(2026, 8, 28, 12, 34, 56, 123456000, time.UTC),
		ID:        mustOperationIntID(t, operations.KindSourceSync, 42),
	}
	cursor, err := encodeOperationCursor(position, parsedFirst.filter, operationTestArchive)
	require.NoError(err)

	second := httptest.NewRequest(http.MethodGet,
		"/api/v1/operations/runs?cursor="+cursor+"&kind=source_sync&limit=99&lane=messages&state=failed", nil)
	parsedSecond, err := parseOperationRunsQuery(second, operationTestArchive)
	require.NoError(err)
	assert.Equal(99, parsedSecond.Query.Limit)
	require.NotNil(parsedSecond.Query.Position)
	assert.Equal(position, *parsedSecond.Query.Position)
	assert.Equal(operationFilterFingerprint(parsedFirst.filter), operationFilterFingerprint(parsedSecond.filter))

	for _, changed := range []string{
		"kind=source_sync&lane=messages&state=partial",
		"kind=source_sync&state=failed",
		"lane=messages&state=failed",
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/operations/runs?"+changed+"&cursor="+cursor, nil)
		_, err = parseOperationRunsQuery(request, operationTestArchive)
		require.ErrorIs(err, errInvalidOperationCursor)
	}

	crossArchive := httptest.NewRequest(http.MethodGet,
		"/api/v1/operations/runs?kind=source_sync&lane=messages&state=failed&cursor="+cursor, nil)
	_, err = parseOperationRunsQuery(crossArchive, "another-archive")
	require.ErrorIs(err, errInvalidOperationCursor)
}

func TestOperationTokensContainOnlyApprovedOpaqueFields(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	id := mustOperationIntID(t, operations.KindSourceSync, 42)
	reference, err := encodeOperationRunReference(id, operationTestArchive)
	require.NoError(err)
	cursor, err := encodeOperationCursor(operations.Position{
		StartedAt: time.Date(2026, 8, 28, 12, 34, 56, 0, time.UTC), ID: id,
	}, operationHistoryFilter{Kind: operations.KindSourceSync}, operationTestArchive)
	require.NoError(err)

	var referenceFields map[string]json.RawMessage
	require.NoError(json.Unmarshal([]byte(operationTokenPayload(t, reference)), &referenceFields))
	assert.ElementsMatch([]string{"kind", "id_type", "int_id", "archive_uid"}, operationMapKeys(referenceFields))
	var cursorFields map[string]json.RawMessage
	require.NoError(json.Unmarshal([]byte(operationTokenPayload(t, cursor)), &cursorFields))
	assert.ElementsMatch([]string{"t", "k", "it", "i", "f", "a"}, operationMapKeys(cursorFields))

	projected := strings.ToLower(reference + " " + cursor + " " + operationTokenPayload(t, reference) + " " + operationTokenPayload(t, cursor))
	for _, forbidden := range []string{
		"checkpoint", "provider_cursor", "source_id", "person_id", "message_id", "account_id",
		"credential", "authorization", "href", "vcard",
	} {
		assert.NotContains(projected, forbidden)
	}
}

func operationVersionedToken(payload string) string {
	return "1." + operationRawToken(payload)
}

func operationVersionedBytes(payload []byte) string {
	return "1." + base64.RawURLEncoding.EncodeToString(payload)
}

func operationInvalidUTF8RunToken() string {
	payload := []byte(`{"kind":"person_sweep","id_type":"text","string_id":"`)
	payload = append(payload, 0xff)
	payload = append(payload, []byte(`","archive_uid":"archive-fixture-01"}`)...)
	return operationVersionedBytes(payload)
}

func operationAlternateBase64Tail(t *testing.T, canonical string) string {
	t.Helper()
	want, err := base64.RawURLEncoding.DecodeString(canonical)
	require.NoError(t, err)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for _, candidate := range alphabet {
		if byte(candidate) == canonical[len(canonical)-1] {
			continue
		}
		mutated := canonical[:len(canonical)-1] + string(candidate)
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(mutated)
		if decodeErr == nil && bytes.Equal(want, decoded) {
			return mutated
		}
	}
	require.Fail(t, "fixture must have alternate noncanonical tail bits")
	return ""
}

func operationRawToken(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

func operationTokenPayload(t *testing.T, token string) string {
	t.Helper()
	prefix, encoded, found := strings.Cut(token, ".")
	require.True(t, found)
	assert.Equal(t, "1", prefix)
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	require.NoError(t, err)
	return string(payload)
}

func mustOperationIntID(t *testing.T, kind operations.Kind, value int64) operations.StableID {
	t.Helper()
	id, err := operations.NewInt64ID(kind, value)
	require.NoError(t, err)
	return id
}

func mustOperationTextID(t *testing.T, kind operations.Kind, value string) operations.StableID {
	t.Helper()
	id, err := operations.NewTextID(kind, value)
	require.NoError(t, err)
	return id
}

func operationMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
