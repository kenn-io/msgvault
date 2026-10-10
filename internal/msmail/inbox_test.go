package msmail

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func microsoftInboxFixture(t *testing.T, omit string, status int) (*InboxProvider, inboxcontrol.Request, *atomic.Int32) {
	t.Helper()
	var mu sync.Mutex
	tags := []string{"Old", "Other"}
	if omit == "at-cap" {
		tags = make([]string, 100)
		for i := range tags {
			tags[i] = fmt.Sprintf("Category-%d", i)
		}
	}
	writes := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, "Bearer synthetic-token", r.Header.Get("Authorization"))
		assert.Contains(t, r.Header.Get("Prefer"), `IdType="ImmutableId"`)
		switch r.URL.Path {
		case "/me":
			account := "mailbox@example.com"
			if omit == "account" {
				account = "other@example.com"
			}
			assert.NoError(t, json.MarshalWrite(w, map[string]string{"mail": account, "userPrincipalName": account}))
		case "/me/mailFolders/inbox":
			assert.NoError(t, json.MarshalWrite(w, map[string]string{"id": "inbox-id"}))
		case "/me/messages/immutable-1":
			if r.Method == http.MethodPatch {
				writes.Add(1)
				assert.Equal(t, `W/"v1"`, r.Header.Get("If-Match"))
				var body map[string][]string
				assert.NoError(t, json.UnmarshalRead(r.Body, &body))
				assert.ElementsMatch(t, []string{"Next", "Other"}, body["categories"])
				assert.Len(t, body, 1, "category intent must not carry read or location changes")
				if status != http.StatusOK {
					w.WriteHeader(status)
					return
				}
				tags = body["categories"]
			}
			body := map[string]any{"id": "immutable-1", "categories": tags, "isRead": false, "parentFolderId": "inbox-id", "@odata.etag": `W/"v1"`}
			if writes.Load() > 0 {
				body["@odata.etag"] = `W/"v2"`
			}
			if omit != "" {
				delete(body, omit)
			}
			assert.NoError(t, json.MarshalWrite(w, body))
		default:
			assert.Fail(t, "unexpected Graph request", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "msmail", SourceIdentifier: "mailbox@example.com", AccountID: "mailbox@example.com"}
	target := inboxcontrol.Target{SourceID: 1, SourceType: "msmail", SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: inboxcontrol.ScopeMessage, ItemID: 7, ProviderID: "immutable-1"}
	client := NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000)
	provider := NewInboxProvider(client, source).WithWriteCapability(inboxcontrol.CapabilitySupported)
	return provider, inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: &emailtags.Change{Add: []string{"Next"}, Remove: []string{"Old"}}, DryRun: true}, writes
}

func TestMicrosoftInboxCategoriesPreserveReadAndLocation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	provider, request, writes := microsoftInboxFixture(t, "", http.StatusOK)
	before, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	projected, err := provider.Preview(t.Context(), request, before)
	requirements.NoError(err)
	assertions.ElementsMatch([]string{"Next", "Other"}, projected.Tags)
	assertions.Equal(before.Read, projected.Read)
	assertions.Equal(before.Inbox, projected.Inbox)
	assertions.Equal(before.Location, projected.Location)
	assertions.Zero(writes.Load())
	request.DryRun = false
	_, err = provider.Dispatch(t.Context(), request, before)
	requirements.NoError(err)
	after, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	requirements.NoError(provider.Verify(request, before, projected, after))
	assertions.Equal(int32(1), writes.Load())
	after.Read = new(true)
	require.ErrorIs(t, provider.Verify(request, before, projected, after), inboxcontrol.ErrOutcomeUnknown)
	after.Read = before.Read
	after.Location = "other-folder"
	require.ErrorIs(t, provider.Verify(request, before, projected, after), inboxcontrol.ErrOutcomeUnknown)
	after.Location = before.Location
	after.Tags = []string{"Next"}
	assertions.ErrorIs(provider.Verify(request, before, projected, after), inboxcontrol.ErrOutcomeUnknown)
}

func TestMicrosoftInboxRequiresCompleteMetadata(t *testing.T) {
	for _, missing := range []string{"categories", "isRead", "parentFolderId", "@odata.etag"} {
		t.Run(missing, func(t *testing.T) {
			provider, request, writes := microsoftInboxFixture(t, missing, http.StatusOK)
			_, err := provider.Observe(t.Context(), request)
			require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
			assert.Zero(t, writes.Load())
		})
	}
}

func TestMicrosoftInboxConditionalWriteOutcomes(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusPreconditionFailed, inboxcontrol.ErrNoWrite},
		{http.StatusServiceUnavailable, inboxcontrol.ErrOutcomeUnknown},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			provider, request, writes := microsoftInboxFixture(t, "", tc.status)
			before, err := provider.Observe(t.Context(), request)
			require.NoError(t, err)
			request.DryRun = false
			_, err = provider.Dispatch(t.Context(), request, before)
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, int32(1), writes.Load())
		})
	}
}

func TestMicrosoftInboxDeniesForeignIdentity(t *testing.T) {
	for _, mismatch := range []string{"account", "id"} {
		t.Run(mismatch, func(t *testing.T) {
			provider, request, writes := microsoftInboxFixture(t, mismatch, http.StatusOK)
			_, err := provider.Observe(t.Context(), request)
			require.ErrorIs(t, err, inboxcontrol.ErrDenied)
			assert.Zero(t, writes.Load())
		})
	}
}

func TestMicrosoftInboxRejectsStaleAndUnadmittedIntent(t *testing.T) {
	for _, scenario := range []string{"stale", "archive", "missing scope", "unknown scope", "no-op"} {
		t.Run(scenario, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			provider, request, writes := microsoftInboxFixture(t, "", http.StatusOK)
			before, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			switch scenario {
			case "stale":
				before.Read = new(true)
			case "archive":
				request.Operation, request.Tags = inboxcontrol.OpArchive, nil
			case "missing scope":
				provider.WithWriteCapability(inboxcontrol.CapabilityPermissionRequired)
			case "unknown scope":
				provider.WithWriteCapability(inboxcontrol.CapabilityUnavailable)
			case "no-op":
				request.Tags = &emailtags.Change{Add: []string{"Other"}}
			}
			request.DryRun = false
			_, err = provider.Dispatch(t.Context(), request, before)
			if scenario == "no-op" {
				requirements.NoError(err)
			} else {
				require.ErrorIs(t, err, inboxcontrol.ErrNoWrite)
			}
			assertions.Zero(writes.Load())
		})
	}
}

func TestMicrosoftInboxRejectsUnverifiableCategoryProjection(t *testing.T) {
	assertions := assert.New(t)

	provider, request, writes := microsoftInboxFixture(t, "at-cap", http.StatusOK)
	before, err := provider.Observe(t.Context(), request)
	require.NoError(t, err)
	_, err = provider.Preview(t.Context(), request, before)
	require.ErrorIs(t, err, inboxcontrol.ErrInvalid)
	request.DryRun = false
	_, err = provider.Dispatch(t.Context(), request, before)
	require.ErrorIs(t, err, inboxcontrol.ErrNoWrite)
	assertions.Zero(writes.Load())
}
