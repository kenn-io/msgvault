package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fastmail"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type cliIdentityProviderInventory struct {
	records []fastmail.Record
	err     error
}

func (i *cliIdentityProviderInventory) ListIdentityRecords(context.Context) ([]fastmail.Record, error) {
	return append([]fastmail.Record(nil), i.records...), i.err
}

type cliIdentityDiscoveryTestStore struct {
	*store.Store

	page             store.IdentityDiscoveryPage
	countErr         error
	scanErr          error
	scanCancel       context.CancelFunc
	scannedSourceIDs []int64
	buildCh          chan bool
	batchFunc        func(
		context.Context,
		int64,
		[]store.IdentityConfirmation,
	) ([]store.IdentityConfirmationOutcome, error)
	listSourcesErr error
}

func (s *cliIdentityDiscoveryTestStore) ListSources(sourceType string) ([]*store.Source, error) {
	if s.listSourcesErr != nil {
		return nil, s.listSourcesErr
	}
	return s.Store.ListSources(sourceType)
}

func (s *cliIdentityDiscoveryTestStore) CountIdentityDiscoveryMessagesContext(
	_ context.Context,
	_ int64,
) (int64, error) {
	if s.countErr != nil {
		return 0, s.countErr
	}
	return s.page.Scanned, nil
}

func (s *cliIdentityDiscoveryTestStore) ScanIdentityDiscoveryPageContext(
	ctx context.Context,
	sourceID, afterID int64,
	_ int,
) (store.IdentityDiscoveryPage, error) {
	s.scannedSourceIDs = append(s.scannedSourceIDs, sourceID)
	if afterID == 0 {
		return s.page, nil
	}
	if s.scanErr != nil {
		return store.IdentityDiscoveryPage{}, s.scanErr
	}
	if s.scanCancel != nil {
		s.scanCancel()
		<-ctx.Done()
		return store.IdentityDiscoveryPage{}, ctx.Err()
	}
	return store.IdentityDiscoveryPage{}, nil
}

func (s *cliIdentityDiscoveryTestStore) BuildCLICache(
	_ context.Context,
	fullRebuild bool,
	_ func(CLICacheBuildEvent) error,
) error {
	s.buildCh <- fullRebuild
	return nil
}

func (s *cliIdentityDiscoveryTestStore) AddAccountIdentitiesBatchContext(
	ctx context.Context,
	sourceID int64,
	confirmations []store.IdentityConfirmation,
) ([]store.IdentityConfirmationOutcome, error) {
	if s.batchFunc != nil {
		return s.batchFunc(ctx, sourceID, confirmations)
	}
	return s.Store.AddAccountIdentitiesBatchContext(ctx, sourceID, confirmations)
}

func newCLIIdentityDiscoveryTestServer(
	t *testing.T,
) (*Server, *cliIdentityDiscoveryTestStore, *store.Source) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "primary@example.test")
	require.NoError(t, err)
	wrapped := &cliIdentityDiscoveryTestStore{
		Store: st,
		page: store.IdentityDiscoveryPage{
			Scanned:     2,
			NextAfterID: 2,
			Observations: []store.IdentityObservation{
				{MessageID: 1, Identifier: "strong@example.test", RecipientType: "from", HasSentFolder: true},
				{MessageID: 2, Identifier: "weak@example.test", RecipientType: "to"},
			},
		},
		buildCh: make(chan bool, 2),
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, wrapped, nil, testLogger())
	return srv, wrapped, source
}

func postDiscoverNDJSON(
	t *testing.T,
	srv *Server,
	body string,
) []identityops.DiscoverEvent {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/identities/discover",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code, "body: %s", resp.Body.String())
	require.Equal(t, "application/x-ndjson", resp.Header().Get("Content-Type"))

	var events []identityops.DiscoverEvent
	decoder := json.NewDecoder(resp.Body)
	for {
		var event identityops.DiscoverEvent
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		events = append(events, event)
	}
	return events
}

func discoverResultEvent(t *testing.T, events []identityops.DiscoverEvent) identityops.DiscoverResult {
	t.Helper()
	require.NotEmpty(t, events)
	require.Equal(t, "result", events[len(events)-1].Type)
	require.NotNil(t, events[len(events)-1].Result)
	return *events[len(events)-1].Result
}

func postIdentityImport(
	t *testing.T,
	srv *Server,
	body string,
) (*httptest.ResponseRecorder, identityops.ImportResult) {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/identities/import",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	var result identityops.ImportResult
	if resp.Code == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	}
	return resp, result
}

func requireIdentityDiscoveryCacheBuild(
	t *testing.T,
	wrapped *cliIdentityDiscoveryTestStore,
) {
	t.Helper()
	select {
	case fullRebuild := <-wrapped.buildCh:
		assert.False(t, fullRebuild)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no background cache build was scheduled")
	}
}

func assertNoIdentityDiscoveryCacheBuild(
	t *testing.T,
	wrapped *cliIdentityDiscoveryTestStore,
) {
	t.Helper()
	select {
	case fullRebuild := <-wrapped.buildCh:
		assert.Fail(t, "unexpected background cache build", "fullRebuild=%v", fullRebuild)
	default:
	}
}

func TestCLIIdentityDiscoverPreviewAndApplyParity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, wrapped, source := newCLIIdentityDiscoveryTestServer(t)

	previewEvents := postDiscoverNDJSON(t, srv, fmt.Sprintf(`{"source_id":%d}`, source.ID))
	preview := discoverResultEvent(t, previewEvents)
	requirements.GreaterOrEqual(len(previewEvents), 2)
	assertions.Equal("progress", previewEvents[0].Type)
	assertions.NotNil(previewEvents[0].Progress)
	assertions.Equal(int64(2), previewEvents[0].Progress.Done)

	applyEvents := postDiscoverNDJSON(t, srv, fmt.Sprintf(`{"source_id":%d,"apply":true}`, source.ID))
	apply := discoverResultEvent(t, applyEvents)
	assertions.Equal(preview.Candidates, apply.Candidates)
	requirements.NotEmpty(apply.Applied, "apply result: %+v", apply)
	assertions.True(apply.Applied[0].Added)
	requireIdentityDiscoveryCacheBuild(t, wrapped)

	repeatEvents := postDiscoverNDJSON(t, srv, fmt.Sprintf(`{"source_id":%d,"apply":true}`, source.ID))
	repeat := discoverResultEvent(t, repeatEvents)
	assertions.Empty(repeat.Applied)
	assertNoIdentityDiscoveryCacheBuild(t, wrapped)
}

func TestCLIIdentityDiscoverProviderPreviewAndApplyUseOneResolvedSource(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, wrapped, source := newCLIIdentityDiscoveryTestServer(t)
	const token = "do-not-log-provider-token"
	srv.cfg.Fastmail = []config.FastmailSource{{SourceID: source.ID, APIToken: token}}
	inventory := &cliIdentityProviderInventory{records: []fastmail.Record{
		{Identifier: "active@example.test", State: "enabled", Kind: "masked-email"},
		{Identifier: "ACTIVE@example.test", State: "pending", Kind: "identity"},
		{Identifier: "old@example.test", State: "disabled", Kind: "masked-email"},
		{Identifier: "deleted@example.test", State: "deleted", Kind: "masked-email"},
		{Identifier: "waiting@example.test", State: "pending", Kind: "masked-email"},
		{Identifier: "*@example.test", State: "enabled", Kind: "identity"},
		{Identifier: "not an address", State: "enabled", Kind: "identity"},
	}}
	srv.fastmailInventoryFactory = func(gotToken string) fastmailIdentityInventory {
		requirements.Equal(token, gotToken)
		return inventory
	}

	preview := discoverResultEvent(t, postDiscoverNDJSON(
		t, srv, fmt.Sprintf(`{"source_id":%d,"provider":true}`, source.ID),
	))
	apply := discoverResultEvent(t, postDiscoverNDJSON(
		t, srv, fmt.Sprintf(`{"source_id":%d,"provider":true,"apply":true}`, source.ID),
	))

	assertions.Equal(preview.Candidates, apply.Candidates)
	encoded, err := json.Marshal(preview)
	requirements.NoError(err)
	var reported struct {
		Candidates []struct {
			Identifier     string   `json:"identifier"`
			ProviderStates []string `json:"provider_states"`
		} `json:"candidates"`
	}
	requirements.NoError(json.Unmarshal(encoded, &reported))
	statesByIdentifier := make(map[string][]string, len(reported.Candidates))
	for _, candidate := range reported.Candidates {
		statesByIdentifier[candidate.Identifier] = candidate.ProviderStates
	}
	assertions.Equal([]string{"enabled", "pending"}, statesByIdentifier["ACTIVE@example.test"])
	assertions.Equal([]string{"deleted"}, statesByIdentifier["deleted@example.test"])
	assertions.Equal([]string{"disabled"}, statesByIdentifier["old@example.test"])
	assertions.Equal([]string{"pending"}, statesByIdentifier["waiting@example.test"])
	assertions.Equal([]string{
		"ACTIVE@example.test",
		"deleted@example.test",
		"old@example.test",
		"strong@example.test",
	}, identityConfirmationIdentifiers(apply.Applied), "pending and malformed provider rows must not apply")
	assertions.Equal([]identityops.RejectedCandidate{
		{Identifier: "*@example.test", Reason: "wildcard identity"},
		{Identifier: "not an address", Reason: "identifier is not a concrete mailbox address"},
	}, preview.Rejected)
	assertions.Equal([]int64{source.ID, source.ID, source.ID, source.ID}, wrapped.scannedSourceIDs)
	requireIdentityDiscoveryCacheBuild(t, wrapped)
}

func TestCLIIdentityDiscoverProviderErrorsAreActionableAndRedacted(t *testing.T) {
	t.Run("missing configuration", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		srv, _, source := newCLIIdentityDiscoveryTestServer(t)
		request := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/cli/identities/discover",
			strings.NewReader(fmt.Sprintf(`{"source_id":%d,"provider":true}`, source.ID)),
		)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()

		srv.Router().ServeHTTP(response, request)

		requirements.Equal(http.StatusBadRequest, response.Code, "body: %s", response.Body.String())
		assertions.Contains(response.Body.String(), fmt.Sprintf("source %d", source.ID))
		assertions.Contains(response.Body.String(), "primary@example.test")
		assertions.Contains(response.Body.String(), "[[fastmail]]")
	})

	t.Run("inventory failure", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		const (
			token          = "do-not-log-provider-token"
			privateAddress = "private-alias@example.test"
		)
		srv, _, source := newCLIIdentityDiscoveryTestServer(t)
		srv.cfg.Fastmail = []config.FastmailSource{{SourceID: source.ID, APIToken: token}}
		inventory := &cliIdentityProviderInventory{err: fmt.Errorf("inventory failed with %s for %s", token, privateAddress)}
		srv.fastmailInventoryFactory = func(string) fastmailIdentityInventory { return inventory }
		var logs bytes.Buffer
		srv.logger = slog.New(slog.NewJSONHandler(&logs, nil))
		request := httptest.NewRequest(
			http.MethodPost,
			"/api/v1/cli/identities/discover",
			strings.NewReader(fmt.Sprintf(`{"source_id":%d,"provider":true}`, source.ID)),
		)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()

		srv.Router().ServeHTTP(response, request)

		requirements.Equal(http.StatusInternalServerError, response.Code, "body: %s", response.Body.String())
		// The public response payload must stay generic no matter what the
		// upstream inventory error contains. The internal log intentionally
		// receives the full diagnostic detail so operators can act on it;
		// see TestCLIIdentityDiscoverInventoryFailureLogsStatusBearingError.
		assertions.NotContains(response.Body.String(), token)
		assertions.NotContains(response.Body.String(), privateAddress)
	})
}

// TestCLIIdentityDiscoverStoreFailureIsInternalNotInvalid confirms that an
// infrastructure failure while resolving [[fastmail]] configuration (e.g. a
// database error listing archive sources) surfaces as an internal server
// error, not a user-input error, since the requester did nothing wrong.
func TestCLIIdentityDiscoverStoreFailureIsInternalNotInvalid(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, wrapped, source := newCLIIdentityDiscoveryTestServer(t)
	srv.cfg.Fastmail = []config.FastmailSource{{SourceID: source.ID, APIToken: "do-not-log-provider-token"}}
	wrapped.listSourcesErr = errors.New("db unavailable: connection reset")
	var logs bytes.Buffer
	srv.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/identities/discover",
		strings.NewReader(fmt.Sprintf(`{"source_id":%d,"provider":true}`, source.ID)),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	srv.Router().ServeHTTP(response, request)

	requirements.Equal(http.StatusInternalServerError, response.Code, "body: %s", response.Body.String())
	assertions.NotContains(response.Body.String(), "db unavailable")
	assertions.NotContains(response.Body.String(), "connection reset")
	// Pin this test to the [[fastmail]] source-lookup branch specifically,
	// so it can't keep passing via some other unrelated 500 path after a
	// future refactor.
	assertions.Contains(logs.String(), "list sources for Fastmail configuration")
	assertions.Contains(logs.String(), "[[fastmail]]")
}

// TestCLIIdentityDiscoverInventoryFailureLogsStatusBearingError confirms that
// the JMAP inventory error, which safely carries method/host/HTTP status
// context useful for operators, reaches the internal log record instead of
// being discarded and replaced with a generic error before logging. The
// public HTTP response body must stay generic regardless.
func TestCLIIdentityDiscoverInventoryFailureLogsStatusBearingError(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	const token = "do-not-log-provider-token"
	srv, _, source := newCLIIdentityDiscoveryTestServer(t)
	srv.cfg.Fastmail = []config.FastmailSource{{SourceID: source.ID, APIToken: token}}
	inventory := &cliIdentityProviderInventory{
		err: errors.New("MaskedEmail/get https://api.fastmail.com/jmap/api/ returned HTTP status 502"),
	}
	srv.fastmailInventoryFactory = func(string) fastmailIdentityInventory { return inventory }
	var logs bytes.Buffer
	srv.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/identities/discover",
		strings.NewReader(fmt.Sprintf(`{"source_id":%d,"provider":true}`, source.ID)),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	srv.Router().ServeHTTP(response, request)

	requirements.Equal(http.StatusInternalServerError, response.Code, "body: %s", response.Body.String())
	assertions.Contains(logs.String(), "502")
	assertions.NotContains(logs.String(), token)
	assertions.NotContains(response.Body.String(), "502")
}

func TestCLIIdentityImportPreviewApplyAndRetryUseParsedEntries(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, wrapped, source := newCLIIdentityDiscoveryTestServer(t)
	body := fmt.Sprintf(`{
		"source_id":%d,
		"signal":"bulk-import",
		"entries":[
			{"identifier":"Old@Example.test","state":"disabled"},
			{"identifier":"waiting@example.test","state":"pending"}
		]
	}`, source.ID)

	previewResp, preview := postIdentityImport(t, srv, body)
	requirements.Equal(http.StatusOK, previewResp.Code, previewResp.Body.String())
	assertions.Empty(preview.Applied)
	assertNoIdentityDiscoveryCacheBuild(t, wrapped)

	applyResp, apply := postIdentityImport(t, srv, strings.Replace(body, "\n\t}", ",\n\t\t\"apply\":true\n\t}", 1))
	requirements.Equal(http.StatusOK, applyResp.Code, applyResp.Body.String())
	assertions.Equal(preview.Candidates, apply.Candidates)
	assertions.Equal([]store.IdentityConfirmationOutcome{
		{Identifier: "Old@Example.test", Added: true, Signals: []string{"bulk-import"}},
		{Identifier: "waiting@example.test", Added: true, Signals: []string{"bulk-import"}},
	}, apply.Applied)
	requireIdentityDiscoveryCacheBuild(t, wrapped)

	retryResp, retry := postIdentityImport(t, srv, strings.Replace(body, "\n\t}", ",\n\t\t\"apply\":true\n\t}", 1))
	requirements.Equal(http.StatusOK, retryResp.Code, retryResp.Body.String())
	assertions.Empty(retry.Applied)
	assertNoIdentityDiscoveryCacheBuild(t, wrapped)
}

func TestCLIIdentityImportRejectsInvalidRowsAndExplicitNonPositiveSourceID(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "invalid later row",
			body: `{"source_id":14,"apply":true,"entries":[{"identifier":"good@example.test"},{"identifier":"not an address"}]}`,
			want: "concrete mailbox address",
		},
		{
			name: "explicit zero",
			body: `{"source_id":0,"entries":[{"identifier":"good@example.test"}]}`,
			want: "source ID must be positive",
		},
		{
			name: "explicit null",
			body: `{"source_id":null,"entries":[{"identifier":"good@example.test"}]}`,
			want: "source ID must be positive",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv, wrapped, _ := newCLIIdentityDiscoveryTestServer(t)
			resp, _ := postIdentityImport(t, srv, test.body)

			assert.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
			assert.Contains(t, resp.Body.String(), test.want)
			assertNoIdentityDiscoveryCacheBuild(t, wrapped)
		})
	}
}

func TestCLIIdentityImportAccountRequiresUniqueSource(t *testing.T) {
	srv, wrapped, source := newCLIIdentityDiscoveryTestServer(t)
	_, err := wrapped.GetOrCreateSource("gmail", source.Identifier)
	require.NoError(t, err)

	resp, _ := postIdentityImport(t, srv, `{
		"account":"primary@example.test",
		"entries":[{"identifier":"alias@example.test"}]
	}`)

	assert.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
	assert.Contains(t, resp.Body.String(), "matches multiple sources")
	assertNoIdentityDiscoveryCacheBuild(t, wrapped)
}

func TestCLIIdentityImportPartialCommitSchedulesCacheBeforeError(t *testing.T) {
	srv, wrapped, source := newCLIIdentityDiscoveryTestServer(t)
	wrapped.batchFunc = func(
		_ context.Context,
		_ int64,
		confirmations []store.IdentityConfirmation,
	) ([]store.IdentityConfirmationOutcome, error) {
		return []store.IdentityConfirmationOutcome{{
			Identifier: confirmations[0].Identifier,
			Added:      true,
			Signals:    confirmations[0].Signals,
		}}, errors.New("second chunk failed")
	}

	resp, _ := postIdentityImport(t, srv, fmt.Sprintf(`{
		"source_id":%d,
		"apply":true,
		"entries":[{"identifier":"first@example.test"},{"identifier":"second@example.test"}]
	}`, source.ID))

	assert.Equal(t, http.StatusInternalServerError, resp.Code, resp.Body.String())
	requireIdentityDiscoveryCacheBuild(t, wrapped)
}

func identityConfirmationIdentifiers(outcomes []store.IdentityConfirmationOutcome) []string {
	identifiers := make([]string, len(outcomes))
	for i, outcome := range outcomes {
		identifiers[i] = outcome.Identifier
	}
	return identifiers
}

func TestCLIIdentityDiscoverAccountAndSourceIDSelection(t *testing.T) {
	assertions := assert.New(t)
	srv, wrapped, source := newCLIIdentityDiscoveryTestServer(t)

	byAccount := discoverResultEvent(t, postDiscoverNDJSON(
		t, srv, `{"account":"primary@example.test"}`,
	))
	assertions.Equal(source.ID, byAccount.SourceID)
	assertions.Equal([]int64{source.ID, source.ID}, wrapped.scannedSourceIDs)

	duplicate, err := wrapped.GetOrCreateSource("gmail", "primary@example.test")
	require.NoError(t, err)
	assertions.NotEqual(source.ID, duplicate.ID)
	wrapped.scannedSourceIDs = nil
	byID := discoverResultEvent(t, postDiscoverNDJSON(
		t, srv, fmt.Sprintf(`{"source_id":%d}`, source.ID),
	))
	assertions.Equal(source.ID, byID.SourceID)
	assertions.Equal([]int64{source.ID, source.ID}, wrapped.scannedSourceIDs)
}

func TestCLIIdentityDiscoverStreamsSanitizedTerminalErrorAfterProgress(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	srv, wrapped, source := newCLIIdentityDiscoveryTestServer(t)
	const secret = "provider-token-private-value"
	wrapped.scanErr = errors.New("provider request failed with " + secret)
	var logs bytes.Buffer
	srv.logger = slog.New(slog.NewTextHandler(&logs, nil))

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/identities/discover",
		strings.NewReader(fmt.Sprintf(`{"source_id":%d}`, source.ID)),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
	requirements.Equal("application/x-ndjson", resp.Header().Get("Content-Type"))
	var events []struct {
		Type  string `json:"type"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	decoder := json.NewDecoder(resp.Body)
	for {
		var event struct {
			Type  string `json:"type"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error,omitempty"`
		}
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		requirements.NoError(err)
		events = append(events, event)
	}
	requirements.Len(events, 2)
	assertions.Equal("progress", events[0].Type)
	assertions.Equal("error", events[1].Type)
	requirements.NotNil(events[1].Error)
	assertions.Equal("internal_error", events[1].Error.Code)
	assertions.Equal("Failed to discover identities", events[1].Error.Message)
	assertions.NotContains(resp.Body.String(), secret)
	assertions.NotContains(logs.String(), secret)
}

func TestCLIIdentityDiscoverRejectsExplicitZeroSourceID(t *testing.T) {
	tests := []string{
		`{"source_id":0}`,
		`{"account":"primary@example.test","source_id":0}`,
	}
	for _, body := range tests {
		t.Run(body, func(t *testing.T) {
			srv, _, _ := newCLIIdentityDiscoveryTestServer(t)
			req := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/cli/identities/discover",
				strings.NewReader(body),
			)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()

			srv.Router().ServeHTTP(resp, req)

			require.Equal(t, http.StatusBadRequest, resp.Code, "body: %s", resp.Body.String())
			var got ErrorResponse
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
			assert.Contains(t, got.Message, "source ID must be positive")
		})
	}
}

func TestCLIIdentityDiscoverAppliesExplicitWeakConfirmation(t *testing.T) {
	srv, _, source := newCLIIdentityDiscoveryTestServer(t)
	events := postDiscoverNDJSON(t, srv, fmt.Sprintf(
		`{"source_id":%d,"apply":true,"confirm":["weak@example.test"]}`,
		source.ID,
	))
	result := discoverResultEvent(t, events)

	require.Len(t, result.Applied, 2)
	assert.Equal(t, "weak@example.test", result.Applied[1].Identifier)
	assert.Equal(t, []string{"manual"}, result.Applied[1].Signals)
}

func TestCLIIdentityDiscoverClassifiesContextErrorsBeforeStreaming(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode string
	}{
		{name: "deadline", err: context.DeadlineExceeded, wantCode: "query_timeout"},
		{name: "canceled", err: context.Canceled, wantCode: "query_canceled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv, wrapped, source := newCLIIdentityDiscoveryTestServer(t)
			wrapped.countErr = test.err
			req := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/cli/identities/discover",
				strings.NewReader(fmt.Sprintf(`{"source_id":%d}`, source.ID)),
			)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()

			srv.Router().ServeHTTP(resp, req)

			require.Equal(t, http.StatusServiceUnavailable, resp.Code, "body: %s", resp.Body.String())
			var got ErrorResponse
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
			assert.Equal(t, test.wantCode, got.Error)
		})
	}
}

func TestCLIIdentityDiscoverCancellationEmitsNoResult(t *testing.T) {
	srv, wrapped, source := newCLIIdentityDiscoveryTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	wrapped.scanCancel = cancel
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/identities/discover",
		strings.NewReader(fmt.Sprintf(`{"source_id":%d}`, source.ID)),
	).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	assert.Contains(t, resp.Body.String(), `"type":"progress"`)
	assert.NotContains(t, resp.Body.String(), `"type":"result"`)
}

func TestCLIIdentityDiscoverPartialApplyErrorSchedulesOneCacheRebuildWithoutResult(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, wrapped, source := newCLIIdentityDiscoveryTestServer(t)
	wrapped.page = store.IdentityDiscoveryPage{
		Scanned:     2,
		NextAfterID: 2,
		Observations: []store.IdentityObservation{
			{MessageID: 1, Identifier: "first@example.test", RecipientType: "from", IsFromMe: true},
			{MessageID: 2, Identifier: "second@example.test", RecipientType: "from", HasSentFolder: true},
		},
	}
	applyErr := errors.New("apply stopped after committed prefix")
	wrapped.batchFunc = func(
		ctx context.Context,
		sourceID int64,
		confirmations []store.IdentityConfirmation,
	) ([]store.IdentityConfirmationOutcome, error) {
		outcomes, err := wrapped.Store.AddAccountIdentitiesBatchContext(ctx, sourceID, confirmations[:1])
		requirements.NoError(err)
		return outcomes, applyErr
	}
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/identities/discover",
		strings.NewReader(fmt.Sprintf(`{"source_id":%d,"apply":true}`, source.ID)),
	)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	assertions.Contains(resp.Body.String(), `"type":"progress"`)
	assertions.NotContains(resp.Body.String(), `"type":"result"`)
	identities, err := wrapped.ListAccountIdentities(source.ID)
	requirements.NoError(err)
	requirements.Len(identities, 1)
	assertions.Equal("first@example.test", identities[0].Address)
	requireIdentityDiscoveryCacheBuild(t, wrapped)
	assertNoIdentityDiscoveryCacheBuild(t, wrapped)
}

func TestCLIIdentityDiscoverCancellationAfterCommitSchedulesOneCacheRebuildWithoutResult(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, wrapped, source := newCLIIdentityDiscoveryTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	wrapped.batchFunc = func(
		ctx context.Context,
		sourceID int64,
		confirmations []store.IdentityConfirmation,
	) ([]store.IdentityConfirmationOutcome, error) {
		outcomes, err := wrapped.Store.AddAccountIdentitiesBatchContext(ctx, sourceID, confirmations)
		requirements.NoError(err)
		cancel()
		return outcomes, nil
	}
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/identities/discover",
		strings.NewReader(fmt.Sprintf(`{"source_id":%d,"apply":true}`, source.ID)),
	).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()

	srv.Router().ServeHTTP(resp, req)

	assertions.Contains(resp.Body.String(), `"type":"progress"`)
	assertions.NotContains(resp.Body.String(), `"type":"result"`)
	identities, err := wrapped.ListAccountIdentities(source.ID)
	requirements.NoError(err)
	requirements.Len(identities, 1)
	assertions.Equal("strong@example.test", identities[0].Address)
	requireIdentityDiscoveryCacheBuild(t, wrapped)
	assertNoIdentityDiscoveryCacheBuild(t, wrapped)
}

func TestCLIIdentityListSourceIDSelectsOneDuplicateAccount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())
	gmail, err := st.GetOrCreateSource("gmail", "shared@example.test")
	require.NoError(err)
	imap, err := st.GetOrCreateSource("imap", "shared@example.test")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(gmail.ID, "gmail-alias@example.test", "manual"))
	require.NoError(st.AddAccountIdentity(imap.ID, "imap-alias@example.test", "manual"))

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/cli/identities?source_id="+strconv.FormatInt(gmail.ID, 10), nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusOK, resp.Code, "body: %s", resp.Body.String())
	var got cliIdentitiesResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&got))
	require.Len(got.Rows, 1)
	assert.Equal(gmail.ID, got.Rows[0].SourceID)
	assert.Equal("gmail-alias@example.test", got.Rows[0].Identifier)
}

func TestCLIIdentityListRejectsSourceIDWithAccount(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())
	source, err := st.GetOrCreateSource("gmail", "shared@example.test")
	require.NoError(err)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/cli/identities?source_id="+strconv.FormatInt(source.ID, 10)+"&account=shared@example.test", nil)
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)

	require.Equal(http.StatusBadRequest, resp.Code, "body: %s", resp.Body.String())
	var got ErrorResponse
	require.NoError(json.NewDecoder(resp.Body).Decode(&got))
	assert.Contains(t, got.Message, "mutually exclusive")
}

func TestCLIIdentityListRejectsExplicitNonPositiveSourceID(t *testing.T) {
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())
	_, err := st.GetOrCreateSource("gmail", "alice@example.test")
	require.NoError(t, err)

	for _, rawQuery := range []string{
		"source_id=0",
		"source_id=0&account=alice@example.test",
	} {
		t.Run(rawQuery, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/identities?"+rawQuery, nil)
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)

			require.Equal(t, http.StatusBadRequest, resp.Code, "body: %s", resp.Body.String())
			var got ErrorResponse
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
			assert.Contains(t, got.Message, "source ID must be positive")
		})
	}
}

func TestCLIIdentityMutationsSourceIDDisambiguateDuplicateAccounts(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())
	gmail, err := st.GetOrCreateSource("gmail", "shared@example.test")
	require.NoError(err)
	imap, err := st.GetOrCreateSource("imap", "shared@example.test")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(imap.ID, "shared-alias@example.test", "manual"))

	addReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/cli/identities",
		strings.NewReader(fmt.Sprintf(`{"source_id":%d,"identifier":"shared-alias@example.test","signal":"manual"}`, gmail.ID)),
	)
	addReq.Header.Set("Content-Type", "application/json")
	addResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(addResp, addReq)
	require.Equal(http.StatusOK, addResp.Code, "body: %s", addResp.Body.String())

	gmailIdentities, err := st.ListAccountIdentities(gmail.ID)
	require.NoError(err)
	require.Len(gmailIdentities, 1)
	imapIdentities, err := st.ListAccountIdentities(imap.ID)
	require.NoError(err)
	require.Len(imapIdentities, 1)

	removeReq := httptest.NewRequest(
		http.MethodDelete,
		"/api/v1/cli/identities",
		strings.NewReader(fmt.Sprintf(`{"source_id":%d,"identifier":"shared-alias@example.test"}`, gmail.ID)),
	)
	removeReq.Header.Set("Content-Type", "application/json")
	removeResp := httptest.NewRecorder()
	srv.Router().ServeHTTP(removeResp, removeReq)
	require.Equal(http.StatusOK, removeResp.Code, "body: %s", removeResp.Body.String())

	gmailIdentities, err = st.ListAccountIdentities(gmail.ID)
	require.NoError(err)
	assert.Empty(t, gmailIdentities)
	imapIdentities, err = st.ListAccountIdentities(imap.ID)
	require.NoError(err)
	require.Len(imapIdentities, 1)
}

func TestCLIIdentityMutationsRejectExplicitNonPositiveSourceIDWithoutMutation(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		body       string
		identifier string
		seed       bool
	}{
		{
			name:       "add zero alone",
			method:     http.MethodPost,
			body:       `{"source_id":0,"identifier":"new@example.test","signal":"manual"}`,
			identifier: "new@example.test",
		},
		{
			name:       "add null with account",
			method:     http.MethodPost,
			body:       `{"account":"primary@example.test","source_id":null,"identifier":"new@example.test","signal":"manual"}`,
			identifier: "new@example.test",
		},
		{
			name:       "remove null alone",
			method:     http.MethodDelete,
			body:       `{"source_id":null,"identifier":"existing@example.test"}`,
			identifier: "existing@example.test",
			seed:       true,
		},
		{
			name:       "remove zero with account",
			method:     http.MethodDelete,
			body:       `{"account":"primary@example.test","source_id":0,"identifier":"existing@example.test"}`,
			identifier: "existing@example.test",
			seed:       true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("imap", "primary@example.test")
			requirements.NoError(err)
			if test.seed {
				requirements.NoError(st.AddAccountIdentity(source.ID, test.identifier, "manual"))
			}
			srv := NewServer(
				&config.Config{Server: config.ServerConfig{APIPort: 8080}},
				st,
				nil,
				testLogger(),
			)

			resp := doCLIIdentityRequest(t, srv, test.method, test.body)

			requirements.Equal(http.StatusBadRequest, resp.Code, "body: %s", resp.Body.String())
			var got ErrorResponse
			requirements.NoError(json.NewDecoder(resp.Body).Decode(&got))
			assertions.Contains(got.Message, "source ID must be positive")
			identities, listErr := st.ListAccountIdentities(source.ID)
			requirements.NoError(listErr)
			if test.seed {
				requirements.Len(identities, 1)
				assertions.Equal(test.identifier, identities[0].Address)
			} else {
				assertions.Empty(identities)
			}
		})
	}
}
