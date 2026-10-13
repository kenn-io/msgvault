package msmail

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type inboxGraph struct {
	mu      sync.Mutex
	message map[string]any
	account string
	profile map[string]any
	writes  int
	reads   int
	status  int
}

func microsoftInboxFixture(t *testing.T) (*InboxProvider, *inboxGraph, inboxcontrol.Request) {
	t.Helper()
	f := &inboxGraph{account: "me@example.com", message: map[string]any{"id": "immutable/1", "categories": []string{"Old", "Other"}, "isRead": false, "parentFolderId": "inbox-id", "@odata.etag": `W/"v1"`}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		assert.Contains(t, r.Header.Get("Prefer"), `IdType="ImmutableId"`)
		switch r.URL.Path {
		case "/me":
			assert.Equal(t, http.MethodGet, r.Method)
			if f.profile != nil {
				assert.NoError(t, json.MarshalWrite(w, f.profile))
			} else {
				assert.NoError(t, json.MarshalWrite(w, map[string]string{"mail": f.account, "userPrincipalName": f.account}))
			}
		case "/me/mailFolders/inbox":
			assert.Equal(t, http.MethodGet, r.Method)
			assert.NoError(t, json.MarshalWrite(w, map[string]string{"id": "inbox-id"}))
		case "/me/messages/immutable/1":
			assert.Equal(t, "/me/messages/immutable%2F1", r.URL.EscapedPath())
			if r.Method == http.MethodPatch {
				f.writes++
				assert.Equal(t, f.message["@odata.etag"], r.Header.Get("If-Match"))
				var body map[string][]string
				if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Len(t, body, 1)
				assert.NotNil(t, body["categories"])
				if f.status != 0 {
					w.WriteHeader(f.status)
					return
				}
				f.message["categories"] = body["categories"]
				f.message["@odata.etag"] = `W/"v2"`
			} else {
				assert.Equal(t, http.MethodGet, r.Method)
				f.reads++
				assert.Equal(t, "id,categories,isRead,parentFolderId", r.URL.Query().Get("$select"))
			}
			assert.NoError(t, json.MarshalWrite(w, f.message))
		default:
			assert.Fail(t, "unexpected Graph endpoint", "%s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: SourceType, SourceIdentifier: "me@example.com", AccountID: "me@example.com"}
	client := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 10000)
	p := NewInboxProvider(client, source).WithWriteCapability(inboxcontrol.CapabilitySupported)
	target := inboxcontrol.Target{SourceID: 1, SourceType: SourceType, SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: inboxcontrol.ScopeMessage, ItemID: 2, ProviderID: "immutable/1"}
	return p, f, inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: &emailtags.MessageTagChange{Add: []string{"Next"}, Remove: []string{"OLD"}}, DryRun: true}
}

func TestMicrosoftInboxMatchesConfiguredMailboxAliases(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile map[string]any
		want    bool
	}{
		{"primary", map[string]any{"mail": "ME@example.com"}, true},
		{"distinct UPN", map[string]any{"mail": "primary@example.com", "userPrincipalName": "ME@example.com"}, true},
		{"SMTP alias", map[string]any{"mail": "primary@example.com", "proxyAddresses": []string{"SMTP:ME@example.com"}}, true},
		{"smtp alias", map[string]any{"proxyAddresses": []string{"smtp:ME@example.com"}}, true},
		{"foreign", map[string]any{"mail": "other@example.com", "userPrincipalName": "other@example.com", "proxyAddresses": []string{"SMTP:other@example.com"}}, false},
		{"non SMTP", map[string]any{"proxyAddresses": []string{"SIP:me@example.com"}}, false},
		{"empty", map[string]any{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			p, f, request := microsoftInboxFixture(t)
			f.profile = tc.profile
			before, err := p.Observe(t.Context(), request)
			if !tc.want {
				requirements.ErrorIs(err, inboxcontrol.ErrDenied)
				assertions.Zero(f.writes)
				assertions.Zero(f.reads, "deny the account before reading the message")
				return
			}
			requirements.NoError(err)
			assertions.Equal("me@example.com", before.Target.AccountID)
			assertions.Equal("me@example.com", before.Target.SourceIdentifier)
			_, err = p.Preview(t.Context(), request, before)
			requirements.NoError(err)
			assertions.Zero(f.writes)
		})
	}
}

func TestMicrosoftInboxCategoriesPreserveReadAndLocation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	p, f, request := microsoftInboxFixture(t)
	before, err := p.Observe(t.Context(), request)
	requirements.NoError(err)
	requirements.NotNil(before.Read)
	assertions.False(*before.Read)
	assertions.Equal("inbox-id", before.Location)
	assertions.Equal(`W/"v1"`, before.Revision)
	projected, err := p.Preview(t.Context(), request, before)
	requirements.NoError(err)
	assertions.ElementsMatch([]string{"Next", "Other"}, projected.Tags)
	assertions.Zero(f.writes)
	request.DryRun = false
	_, err = p.Dispatch(t.Context(), request, before)
	requirements.NoError(err)
	assertions.Equal(1, f.writes)
	assertions.Equal(1, f.reads, "dispatch must not replace the signed snapshot with a fresh GET")
	after, err := p.Observe(t.Context(), request)
	requirements.NoError(err)
	requirements.NoError(p.Verify(request, before, projected, after))
	assertions.Equal(2, f.reads, "verification uses independent complete readback")
	assertions.False(*after.Read)
	assertions.Equal(before.Location, after.Location)
	_, err = p.Dispatch(t.Context(), request, after)
	requirements.NoError(err)
	assertions.Equal(1, f.writes, "no-op does not PATCH")
}

func TestMicrosoftInboxRequiresCompleteMetadata(t *testing.T) {
	for _, field := range []string{"categories", "isRead", "parentFolderId", "@odata.etag", "id"} {
		for _, null := range []bool{false, true} {
			t.Run(field+map[bool]string{false: " omitted", true: " null"}[null], func(t *testing.T) {
				p, f, request := microsoftInboxFixture(t)
				if null {
					f.message[field] = nil
				} else {
					delete(f.message, field)
				}
				_, err := p.Observe(t.Context(), request)
				require.Error(t, err)
				assert.Zero(t, f.writes)
			})
		}
	}
	p, f, request := microsoftInboxFixture(t)
	f.message["categories"] = []string{}
	state, err := p.Observe(t.Context(), request)
	require.NoError(t, err)
	assert.NotNil(t, state.Tags, "known empty remains an explicit complete snapshot in memory")
	assert.Empty(t, state.Tags)
}

func TestMicrosoftInboxConditionalWriteOutcomes(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 412, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assertions := assert.New(t)
			p, f, request := microsoftInboxFixture(t)
			before, err := p.Observe(t.Context(), request)
			require.NoError(t, err)
			f.status = status
			_, err = p.Dispatch(t.Context(), request, before)
			if status >= 500 {
				require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
			} else {
				require.ErrorIs(t, err, inboxcontrol.ErrNoWrite)
			}
			assertions.Equal(1, f.writes)
		})
	}
}

func TestMicrosoftInboxDeniesForeignIdentityAndUnsupportedWrites(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	p, f, request := microsoftInboxFixture(t)
	f.account = "other@example.com"
	_, err := p.Observe(t.Context(), request)
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	f.account = "me@example.com"
	before, err := p.Observe(t.Context(), request)
	requirements.NoError(err)
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpMove, inboxcontrol.OpCreateFolder} {
		request.Operation = op
		_, err := p.Preview(t.Context(), request, before)
		requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
		_, err = p.Dispatch(t.Context(), request, before)
		requirements.ErrorIs(err, inboxcontrol.ErrNoWrite)
	}
	assertions.Zero(f.writes)
}

func TestMicrosoftInboxRejectsUnverifiableCategoryProjection(t *testing.T) {
	p, _, request := microsoftInboxFixture(t)
	before, err := p.Observe(t.Context(), request)
	require.NoError(t, err)
	projected, err := p.Preview(t.Context(), request, before)
	require.NoError(t, err)
	for _, mutate := range []func(*inboxcontrol.State){
		func(s *inboxcontrol.State) { s.Tags = []string{"Next"} },
		func(s *inboxcontrol.State) { s.Tags = []string{"Next", "Other", "Old"} },
		func(s *inboxcontrol.State) { s.Read = new(true) },
		func(s *inboxcontrol.State) { s.Location = "archive" },
		func(s *inboxcontrol.State) { s.Revision = "" },
		func(s *inboxcontrol.State) { s.Target.ProviderID = "other" },
	} {
		after := projected
		mutate(&after)
		assert.ErrorIs(t, p.Verify(request, before, projected, after), inboxcontrol.ErrOutcomeUnknown)
	}
}

func TestMicrosoftInboxCapabilitiesRequireCurrentWriteEvidence(t *testing.T) {
	for _, evidence := range []inboxcontrol.CapabilityStatus{"", inboxcontrol.CapabilityUnavailable, inboxcontrol.CapabilityPermissionRequired, inboxcontrol.CapabilitySupported} {
		t.Run(string(evidence), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			p, f, request := microsoftInboxFixture(t)
			p.WithWriteCapability(evidence)
			capabilities, err := p.Capabilities(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &p.source})
			requirements.NoError(err)
			assertions.False(capabilities.ConditionalWrite, "sending If-Match does not establish a service guarantee")
			for _, capability := range capabilities.Operations {
				switch capability.Operation {
				case inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState:
					assertions.Equal(inboxcontrol.CapabilitySupported, capability.Status)
				case inboxcontrol.OpTags:
					want := evidence
					if evidence == "" {
						want = inboxcontrol.CapabilityUnavailable
					}
					assertions.Equal(want, capability.Status)
				default:
					assertions.Equal(inboxcontrol.CapabilityUnsupported, capability.Status)
				}
			}
			before, err := p.Observe(t.Context(), request)
			requirements.NoError(err)
			_, err = p.Preview(t.Context(), request, before)
			if evidence == inboxcontrol.CapabilitySupported {
				requirements.NoError(err)
			} else {
				require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
				_, err = p.Dispatch(t.Context(), request, before)
				require.ErrorIs(t, err, inboxcontrol.ErrNoWrite)
			}
			assertions.Zero(f.writes)
		})
	}
}

func TestMicrosoftInboxReadbackCategoryOrder(t *testing.T) {
	p, _, request := microsoftInboxFixture(t)
	before, err := p.Observe(t.Context(), request)
	require.NoError(t, err)
	projected, err := p.Preview(t.Context(), request, before)
	require.NoError(t, err)
	after := projected
	after.Tags = []string{"Next", "Other"}
	require.NoError(t, p.Verify(request, before, projected, after), "categories are a collection; provider order is not a mutation")
}

func FuzzMicrosoftInboxCategoryOrder(f *testing.F) {
	f.Add("Next", "Other")
	f.Add("", "")
	f.Add("東京", "Work")
	f.Fuzz(func(t *testing.T, a, b string) {
		assert.True(t, sameCategories([]string{a, b}, []string{b, a}), "category equality must ignore provider ordering")
	})
}
