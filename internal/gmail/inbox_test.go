package gmail

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func inboxGmailSource() inboxcontrol.SourceIdentity {
	return inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.com", AccountID: "owner@example.com"}
}
func inboxGmailTarget() inboxcontrol.Target {
	s := inboxGmailSource()
	return inboxcontrol.Target{SourceID: s.SourceID, SourceType: s.SourceType, SourceIdentifier: s.SourceIdentifier, AccountID: s.AccountID, Scope: inboxcontrol.ScopeMessage, ItemID: 1, ProviderID: "message-1"}
}

func TestInboxGmailNativeDeltas(t *testing.T) {
	for _, tc := range []struct {
		name                string
		op                  inboxcontrol.Operation
		add, remove         []string
		tags                *emailtags.Change
		origin, destination *inboxcontrol.Folder
	}{
		{name: "explicit label move", op: inboxcontrol.OpMove, add: []string{"LabelNew"}, remove: []string{"LabelOld"}, origin: &inboxcontrol.Folder{ID: "LabelOld"}, destination: &inboxcontrol.Folder{ID: "LabelNew"}},
		{name: "archive", op: inboxcontrol.OpArchive, remove: []string{"INBOX"}},
		{name: "unarchive", op: inboxcontrol.OpUnarchive, add: []string{"INBOX"}},
		{name: "read", op: inboxcontrol.OpSetRead, remove: []string{"UNREAD"}},
		{name: "unread", op: inboxcontrol.OpSetUnread, add: []string{"UNREAD"}},
		{name: "retained tagging", op: inboxcontrol.OpTags, add: []string{"LabelNew"}, remove: []string{"LabelOld"}, tags: &emailtags.Change{Add: []string{"LabelNew"}, Remove: []string{"LabelOld"}}},
		{name: "tag removal added concurrently", op: inboxcontrol.OpTags, add: []string{"LabelNew"}, tags: &emailtags.Change{Add: []string{"LabelNew"}, Remove: []string{"ConcurrentLabel"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			labels := []string{"INBOX", "UNREAD", "STARRED", "LabelOld", "LabelOther"}
			for _, id := range tc.add {
				labels = slices.DeleteFunc(labels, func(v string) bool { return v == id })
			}
			posts := 0
			gets := 0
			client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/gmail/v1/users/me/profile":
					_, _ = w.Write([]byte(`{"emailAddress":"owner@example.com","historyId":"12"}`))
				case "/gmail/v1/users/me/labels":
					_, _ = w.Write([]byte(`{"labels":[{"id":"INBOX","name":"Inbox","type":"system"},{"id":"LabelOld","name":"Old","type":"user"},{"id":"LabelNew","name":"New","type":"user"},{"id":"LabelOther","name":"Other","type":"user"},{"id":"ConcurrentLabel","name":"Concurrent","type":"user"}]}`))
				case "/gmail/v1/users/me/messages/message-1":
					gets++
					assert.Equal(t, http.MethodGet, r.Method)
					assert.Equal(t, "metadata", r.URL.Query().Get("format"))
					assert.NotContains(t, r.URL.Query().Get("fields"), "payload")
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "message-1", "labelIds": labels, "historyId": "12"})
				case "/gmail/v1/users/me/messages/message-1/modify":
					posts++
					assert.Equal(t, http.MethodPost, r.Method)
					var delta struct {
						Add    []string `json:"addLabelIds"`
						Remove []string `json:"removeLabelIds"`
					}
					if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&delta)) {
						return
					}
					assert.ElementsMatch(t, tc.add, delta.Add)
					assert.ElementsMatch(t, tc.remove, delta.Remove)
					labels = emailtags.Project(labels, emailtags.Change{Add: delta.Add, Remove: delta.Remove}, false)
					// Another actor adds a label after dispatch. Never restore a full snapshot.
					labels = append(labels, "ConcurrentLabel")
					_, _ = w.Write([]byte(`{"id":"message-1"}`))
				default:
					assert.Fail(t, "unexpected Gmail request", r.URL.String())
					http.NotFound(w, r)
				}
			}))
			provider := NewInboxProvider(client, inboxGmailSource())
			target := inboxGmailTarget()
			request := inboxcontrol.Request{Operation: tc.op, Target: &target, Tags: tc.tags, OriginFolder: tc.origin, Destination: tc.destination, DryRun: true}
			before, err := provider.Observe(context.Background(), request)
			requirements.NoError(err)
			projected, err := provider.Preview(context.Background(), request, before)
			requirements.NoError(err)
			assertions.Equal(0, posts)
			_, err = provider.Dispatch(context.Background(), request, before)
			requirements.NoError(err)
			after, err := provider.Observe(context.Background(), request)
			requirements.NoError(err)
			if tc.name == "tag removal added concurrently" {
				requirements.ErrorIs(provider.Verify(request, before, projected, after), inboxcontrol.ErrOutcomeUnknown)
			} else {
				requirements.NoError(provider.Verify(request, before, projected, after))
			}
			assertions.Equal(1, posts)
			assertions.Equal(2, gets)
			assertions.Contains(after.Tags, "LabelOther")
			assertions.Contains(after.Tags, "STARRED")
			assertions.Contains(after.Tags, "ConcurrentLabel")
			if tc.op == inboxcontrol.OpTags {
				assertions.True(*after.Inbox)
				assertions.False(*after.Read)
			}
		})
	}
}

func TestInboxGmailRejectsUnprovenState(t *testing.T) {
	for _, tc := range []struct{ name, profile, message string }{
		{"wrong account", `{"emailAddress":"other@example.com"}`, `{"id":"message-1","labelIds":[],"historyId":"12"}`},
		{"wrong message", `{"emailAddress":"owner@example.com"}`, `{"id":"message-2","labelIds":[],"historyId":"12"}`},
		{"missing labels", `{"emailAddress":"owner@example.com"}`, `{"id":"message-1","historyId":"12"}`},
		{"null labels", `{"emailAddress":"owner@example.com"}`, `{"id":"message-1","labelIds":null,"historyId":"12"}`},
		{"missing history", `{"emailAddress":"owner@example.com"}`, `{"id":"message-1","labelIds":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				if r.URL.Path == "/gmail/v1/users/me/profile" {
					_, _ = w.Write([]byte(tc.profile))
					return
				}
				_, _ = w.Write([]byte(tc.message))
			}))
			provider := NewInboxProvider(client, inboxGmailSource())
			target := inboxGmailTarget()
			_, err := provider.Observe(context.Background(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
			require.Error(t, err)
		})
	}
}

func TestInboxGmailRejectsUnavailableTagsAndLostWrite(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	posts := 0
	client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			_, _ = w.Write([]byte(`{"emailAddress":"owner@example.com"}`))
		case "/gmail/v1/users/me/labels":
			_, _ = w.Write([]byte(`{"labels":[{"id":"INBOX","name":"Inbox","type":"system"}]}`))
		case "/gmail/v1/users/me/messages/message-1":
			_, _ = w.Write([]byte(`{"id":"message-1","labelIds":["INBOX"],"historyId":"12"}`))
		case "/gmail/v1/users/me/messages/message-1/modify":
			posts++
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	provider := NewInboxProvider(client, inboxGmailSource())
	target := inboxGmailTarget()
	request := inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: &emailtags.Change{Add: []string{"Missing"}}, DryRun: true}
	before, err := provider.Observe(context.Background(), request)
	requirements.NoError(err)
	_, err = provider.Preview(context.Background(), request, before)
	requirements.Error(err)
	assertions.Equal(0, posts)
	request.Operation = inboxcontrol.OpArchive
	request.Tags = nil
	_, err = provider.Dispatch(context.Background(), request, before)
	requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
	assertions.Equal(1, posts)
}

func TestInboxGmailMoveRejectsUnselectedOrSystemLabels(t *testing.T) {
	client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			_, _ = w.Write([]byte(`{"emailAddress":"owner@example.com"}`))
		case "/gmail/v1/users/me/messages/message-1":
			_, _ = w.Write([]byte(`{"id":"message-1","labelIds":["INBOX","LabelOld"],"historyId":"12"}`))
		case "/gmail/v1/users/me/labels":
			_, _ = w.Write([]byte(`{"labels":[{"id":"INBOX","name":"Inbox","type":"system"},{"id":"LabelOld","name":"Old","type":"user"},{"id":"LabelNew","name":"New","type":"user"}]}`))
		default:
			assert.Fail(t, "unexpected Gmail request", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	provider := NewInboxProvider(client, inboxGmailSource())
	target := inboxGmailTarget()
	before, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
	require.NoError(t, err)
	for _, tc := range []struct{ name, origin, destination string }{
		{"missing selection", "", "LabelNew"}, {"unselected origin", "LabelNew", "LabelOld"}, {"unavailable destination", "LabelOld", "Missing"}, {"system destination", "LabelOld", "INBOX"}, {"system origin", "INBOX", "LabelNew"}, {"same location", "LabelOld", "LabelOld"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := inboxcontrol.Request{Operation: inboxcontrol.OpMove, Target: &target, OriginFolder: &inboxcontrol.Folder{ID: tc.origin}, Destination: &inboxcontrol.Folder{ID: tc.destination}, DryRun: true}
			_, err := provider.Preview(t.Context(), request, before)
			assert.Error(t, err)
		})
	}
}

func TestInboxGmailMetadataBatchRequiresAuthoritativeMarkers(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"known empty", `{"id":"metadata-1","labelIds":[],"historyId":"12"}`, true},
		{"wrong identity", `{"id":"metadata-other","labelIds":[],"historyId":"12"}`, false},
		{"missing labels", `{"id":"metadata-1","historyId":"12"}`, false},
		{"missing revision", `{"id":"metadata-1","labelIds":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				assert.Equal(t, "/gmail/v1/users/me/messages/metadata-1", r.URL.Path)
				assert.Equal(t, "metadata", r.URL.Query().Get("format"))
				assert.Equal(t, "id,labelIds,historyId", r.URL.Query().Get("fields"))
				_, _ = w.Write([]byte(tc.body))
			}))
			results, err := client.GetMessageLabelsBatch(t.Context(), []string{"metadata-1"})
			requirements.NoError(err)
			requirements.Len(results, 1)
			if tc.valid {
				requirements.NoError(results[0].Err)
				assertions.Equal(uint64(12), results[0].HistoryID)
				assertions.NotNil(results[0].LabelIDs)
			} else {
				assertions.Error(results[0].Err)
			}
		})
	}
}

func TestInboxGmailFolderCreationUsesNativeIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	labels := []map[string]string{{"id": "LabelExisting", "name": "Existing", "type": "user"}}
	posts := 0
	client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			_, _ = w.Write([]byte(`{"emailAddress":"owner@example.com"}`))
		case "/gmail/v1/users/me/labels":
			if r.Method == http.MethodPost {
				posts++
				var body struct {
					Name string `json:"name"`
				}
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
					return
				}
				assert.Equal(t, "Review", body.Name)
				created := map[string]string{"id": "LabelCreated", "name": body.Name, "type": "user"}
				labels = append(labels, created)
				_ = json.NewEncoder(w).Encode(created)
				return
			}
			assert.Equal(t, http.MethodGet, r.Method)
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": labels})
		default:
			assert.Fail(t, "unexpected Gmail request", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	provider := NewInboxProvider(client, inboxGmailSource())
	source := inboxGmailSource()
	request := inboxcontrol.Request{Operation: inboxcontrol.OpCreateFolder, Source: &source, Destination: &inboxcontrol.Folder{Name: "Review"}, DryRun: true}
	before, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	projected, err := provider.Preview(t.Context(), request, before)
	requirements.NoError(err)
	assertions.Equal(0, posts)
	dispatch, err := provider.Dispatch(t.Context(), request, before)
	requirements.NoError(err)
	requirements.NotNil(dispatch.Folder)
	assertions.Equal("LabelCreated", dispatch.Folder.ID)
	request.ResolvedFolder = dispatch.Folder
	after, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	requirements.NoError(provider.Verify(request, before, projected, after))
	assertions.Equal(1, posts)
	assertions.Contains(after.Folders, inboxcontrol.Folder{ID: "LabelExisting", Name: "Existing"})
	// Ensuring an already existing label is a verified no-op, never another POST.
	request.ResolvedFolder = nil
	before, err = provider.Observe(t.Context(), request)
	requirements.NoError(err)
	projected, err = provider.Preview(t.Context(), request, before)
	requirements.NoError(err)
	dispatch, err = provider.Dispatch(t.Context(), request, before)
	requirements.NoError(err)
	requirements.NotNil(dispatch.Folder)
	request.ResolvedFolder = dispatch.Folder
	after, err = provider.Observe(t.Context(), request)
	requirements.NoError(err)
	requirements.NoError(provider.Verify(request, before, projected, after))
	assertions.Equal(1, posts)
}

func TestInboxGmailMetadataBatchPreservesGoneCause(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	results, err := client.GetMessageLabelsBatch(t.Context(), []string{"gone"})
	requirements.NoError(err)
	requirements.Len(results, 1)
	require.ErrorIs(t, results[0].Err, ErrMessageGone)
	var gone *NotFoundError
	assertions.ErrorAs(results[0].Err, &gone)
}

func TestInboxGmailFolderCreationThroughController(t *testing.T) {
	for _, mode := range []string{"verified", "readback lost", "mapping lost"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			archive := storetest.New(t)
			source := inboxGmailSource()
			source.SourceID, source.SourceIdentifier, source.AccountID = archive.Source.ID, archive.Source.Identifier, archive.Source.Identifier
			labels := []map[string]string{}
			posts := 0
			failRead := false
			client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/gmail/v1/users/me/profile":
					_ = json.NewEncoder(w).Encode(map[string]string{"emailAddress": source.AccountID})
				case "/gmail/v1/users/me/labels":
					if r.Method == http.MethodPost {
						posts++
						created := map[string]string{"id": "LabelCreated", "name": "Review", "type": "user"}
						labels = append(labels, created)
						if mode == "mapping lost" {
							_, _ = w.Write([]byte(`{}`))
							return
						}
						failRead = mode == "readback lost"
						_ = json.NewEncoder(w).Encode(created)
					} else if failRead {
						failRead = false
						w.WriteHeader(http.StatusNotFound)
					} else {
						_ = json.NewEncoder(w).Encode(map[string]any{"labels": labels})
					}
				default:
					assert.Fail(t, "unexpected Gmail request", r.URL.String())
					http.NotFound(w, r)
				}
			}))
			provider := NewInboxProvider(client, source)
			service := inboxcontrol.Service{Ledger: archive.Store, Key: []byte(strings.Repeat("k", 32)),
				Authorize: func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil },
				Resolve:   func(context.Context, inboxcontrol.Request) (inboxcontrol.Provider, error) { return provider, nil },
				AcquireSource: func(ctx context.Context, id int64) (func(), error) {
					lease, err := archive.Store.AcquireSyncExecutionContext(ctx, id)
					if err != nil {
						return nil, err
					}
					return func() { _ = lease.Release() }, nil
				},
				ReconcileState: func(ctx context.Context, before, after inboxcontrol.State) error {
					return archive.Store.ReconcileInboxProviderState(ctx, before.Target, before, after)
				},
			}
			gate := func(context.Context) (func(), error) { return func() {}, nil }
			principal := inboxcontrol.Principal{ID: "folder-owner", Owner: true}
			request := inboxcontrol.Request{Operation: inboxcontrol.OpCreateFolder, Source: &source, Destination: &inboxcontrol.Folder{Name: "Review"}, DryRun: true}
			preview, err := service.Control(t.Context(), request, principal, gate)
			requirements.NoError(err)
			assertions.Equal(0, posts)
			request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "create-review"
			result, err := service.Control(t.Context(), request, principal, gate)
			requirements.NotNil(result)
			requirements.NotNil(result.Receipt)
			if mode == "verified" {
				requirements.NoError(err)
			} else {
				requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
				assertions.Equal(inboxcontrol.StatusUnknown, result.Receipt.Status)
				if mode == "readback lost" {
					requirements.NotNil(result.Receipt.After)
					requirements.NotNil(result.Receipt.After.ProvisionedFolder)
					assertions.Equal("LabelCreated", result.Receipt.After.ProvisionedFolder.ID)
				}
				result, err = service.Control(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: result.Receipt.ID}, principal, gate)
				if mode == "mapping lost" {
					requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
					assertions.Equal(1, posts)
					return
				}
				requirements.NoError(err)
			}
			assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
			var nativeID string
			requirements.NoError(archive.Store.DB().QueryRow(archive.Store.Rebind("SELECT source_label_id FROM labels WHERE source_id = ? AND name = ?"), source.SourceID, "Review").Scan(&nativeID))
			assertions.Equal("LabelCreated", nativeID)
			_, err = service.Control(t.Context(), request, principal, gate)
			requirements.NoError(err)
			assertions.Equal(1, posts)
		})
	}
}

func TestInboxGmailFolderPreviewRejectsUnprovedCatalog(t *testing.T) {
	for _, body := range []string{`{}`, `{"labels":null}`, `{"labels":[{"id":"LabelA","name":"Review","type":"user"},{"id":"LabelB","name":"Review","type":"user"}]}`, `{"labels":[{"name":"Review","type":"user"}]}`} {
		t.Run(body, func(t *testing.T) {
			posts := 0
			client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					posts++
					http.Error(w, "unexpected write", http.StatusInternalServerError)
					return
				}
				if r.URL.Path == "/gmail/v1/users/me/profile" {
					_, _ = w.Write([]byte(`{"emailAddress":"owner@example.com"}`))
				} else {
					_, _ = w.Write([]byte(body))
				}
			}))
			source := inboxGmailSource()
			provider := NewInboxProvider(client, source)
			_, err := provider.Observe(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpCreateFolder, Source: &source, Destination: &inboxcontrol.Folder{Name: "Review"}, DryRun: true})
			require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
			assert.Equal(t, 0, posts)
		})
	}
}

func TestInboxGmailCapabilitiesAreReadOnlyAndNonconditional(t *testing.T) {
	for _, writeStatus := range []inboxcontrol.CapabilityStatus{inboxcontrol.CapabilitySupported, inboxcontrol.CapabilityPermissionRequired, inboxcontrol.CapabilityUnavailable} {
		t.Run(string(writeStatus), func(t *testing.T) {
			assertions := assert.New(t)

			writes := 0
			client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
					http.Error(w, "unexpected write", http.StatusInternalServerError)
					return
				}
				switch r.URL.Path {
				case "/gmail/v1/users/me/profile":
					_, _ = w.Write([]byte(`{"emailAddress":"owner@example.com"}`))
				case "/gmail/v1/users/me/labels":
					_, _ = w.Write([]byte(`{"labels":[]}`))
				default:
					http.NotFound(w, r)
				}
			}))
			source := inboxGmailSource()
			provider := NewInboxProvider(client, source).WithWriteCapability(writeStatus)
			result, err := provider.Capabilities(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source})
			require.NoError(t, err)
			assertions.Equal(source, result.Source)
			assertions.Equal("labels", result.LocationModel)
			assertions.False(result.ConditionalWrite)
			assertions.False(result.ObservedAt.IsZero())
			statuses := map[inboxcontrol.Operation]inboxcontrol.CapabilityStatus{}
			for _, capability := range result.Operations {
				statuses[capability.Operation] = capability.Status
			}
			assertions.Equal(inboxcontrol.CapabilitySupported, statuses[inboxcontrol.OpGetState])
			assertions.Equal(inboxcontrol.CapabilitySupported, statuses[inboxcontrol.OpListFolders])
			for _, op := range []inboxcontrol.Operation{inboxcontrol.OpTags, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpMove, inboxcontrol.OpCreateFolder} {
				assertions.Equal(writeStatus, statuses[op])
			}
			assertions.Equal(0, writes)
		})
	}
}
