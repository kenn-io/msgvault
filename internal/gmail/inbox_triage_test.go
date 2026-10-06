package gmail

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type triagePreviewFixture struct {
	archive   *storetest.Fixture
	source    inboxcontrol.SourceIdentity
	service   *inboxcontrol.Service
	input     inboxcontrol.TriageInput
	principal inboxcontrol.Principal
	now       time.Time
	writes    atomic.Int64
	labels    []string
	onRead    func()
	denied    atomic.Bool
}

func newTriagePreviewFixture(t *testing.T) *triagePreviewFixture {
	t.Helper()
	a := storetest.New(t)
	f := &triagePreviewFixture{archive: a, now: time.Now().UTC(), labels: []string{"INBOX", "UNREAD", "STARRED", "Unrelated"}, principal: inboxcontrol.Principal{ID: "delegate-fixture"}}
	f.source = inboxcontrol.SourceIdentity{SourceID: a.Source.ID, SourceType: "gmail", SourceIdentifier: a.Source.Identifier, AccountID: a.Source.Identifier}
	target := inboxcontrol.Target{SourceID: a.Source.ID, SourceType: "gmail", SourceIdentifier: a.Source.Identifier, AccountID: a.Source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: a.CreateMessage("triage-message"), ProviderID: "triage-message"}
	f.input = inboxcontrol.TriageInput{Source: f.source, Items: []inboxcontrol.TriageItemInput{{Target: target, Categories: []string{"todo"}, EvidenceMessageIDs: []int64{target.ItemID}, IdempotencyKey: "triage-item-1"}}}
	_, err := a.Store.ObserveInboxState(t.Context(), inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), Tags: slices.Clone(f.labels), Revision: "12", ObservedAt: f.now})
	require.NoError(t, err)
	_, err = a.Store.ReplaceInboxTriageMappings(t.Context(), f.source, map[string]string{"todo": "TodoLabel", "reply-needed": "ReplyLabel", "watch": "WatchLabel", "delegated": "DelegatedLabel", "finished": "FinishedLabel", "uncertain": "UncertainLabel"}, 0, inboxcontrol.Principal{ID: "owner-fixture", Owner: true})
	require.NoError(t, err)
	f.service = &inboxcontrol.Service{Ledger: a.Store, Key: []byte(strings.Repeat("k", 32)), Now: func() time.Time { return f.now },
		Authorize: func(_ context.Context, p inboxcontrol.Principal, r inboxcontrol.Request) error {
			if f.denied.Load() || p != f.principal {
				return inboxcontrol.ErrDenied
			}
			return nil
		},
		Resolve: func(ctx context.Context, r inboxcontrol.Request) (inboxcontrol.Provider, error) {
			if r.Target != nil {
				if err := a.Store.ValidateInboxTargetContext(ctx, *r.Target); err != nil {
					return nil, err
				}
			}
			client := newDraftTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					f.writes.Add(1)
					http.Error(w, "write forbidden", 405)
					return
				}
				switch {
				case r.URL.Path == "/gmail/v1/users/me/profile":
					_, err := fmt.Fprintf(w, `{"emailAddress":%q}`, a.Source.Identifier)
					assert.NoError(t, err)
				case strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/messages/triage-message"):
					if f.onRead != nil {
						f.onRead()
					}
					assert.Equal(t, "metadata", r.URL.Query().Get("format"))
					data, err := json.Marshal(map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/"), "labelIds": f.labels, "historyId": "12"})
					assert.NoError(t, err)
					_, err = w.Write(data)
					assert.NoError(t, err)
				case r.URL.Path == "/gmail/v1/users/me/labels":
					_, err := fmt.Fprint(w, `{"labels":[{"id":"TodoLabel","name":"Todo","type":"user"},{"id":"ReplyLabel","name":"Reply","type":"user"},{"id":"WatchLabel","name":"Watch","type":"user"},{"id":"DelegatedLabel","name":"Delegated","type":"user"},{"id":"FinishedLabel","name":"Finished","type":"user"},{"id":"UncertainLabel","name":"Uncertain","type":"user"}]}`)
					assert.NoError(t, err)
				default:
					assert.Fail(t, "unexpected native lookup", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			return NewInboxProvider(client, f.source), nil
		},
	}
	return f
}

func TestInboxGmailTriagePreviewRetainsInboxWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name           string
		categories     []string
		add            []string
		classification string
	}{
		{"todo", []string{"todo"}, []string{"TodoLabel"}, "todo"},
		{"reply", []string{"reply-needed"}, []string{"ReplyLabel"}, "reply-needed"},
		{"watch overrides finished", []string{"finished", "watch"}, []string{"WatchLabel"}, "watch"},
		{"delegated overrides finished", []string{"delegated", "finished"}, []string{"DelegatedLabel"}, "delegated"},
		{"unknown", nil, []string{"UncertainLabel"}, "uncertain"},
		{"conflict", []string{"finished", "todo", "reply-needed"}, []string{"ReplyLabel", "TodoLabel"}, "conflicting"},
		{"finished", []string{"finished"}, []string{"FinishedLabel"}, "finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTriagePreviewFixture(t)
			f.input.Items[0].Categories = tc.categories
			proposal, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
			require.NoError(t, err)
			require.Len(t, proposal.Items, 1)
			item := proposal.Items[0]
			assert.Equal(t, tc.classification, item.Classification)
			assert.True(t, item.RetainInbox)
			assert.Equal(t, inboxcontrol.OpTags, item.Request.Operation)
			require.NotNil(t, item.Request.Tags)
			assert.ElementsMatch(t, tc.add, item.Request.Tags.Add)
			assert.Empty(t, item.Request.Tags.Remove)
			require.NotNil(t, item.Request.Expected)
			require.NotNil(t, item.Projected.Inbox)
			require.NotNil(t, item.Projected.Read)
			assert.True(t, *item.Projected.Inbox)
			assert.False(t, *item.Projected.Read)
			assert.Contains(t, item.Projected.Tags, "STARRED")
			assert.Contains(t, item.Projected.Tags, "Unrelated")
			assert.Equal(t, "triage-item-1", item.Request.IdempotencyKey)
			assert.NoError(t, inboxcontrol.VerifyTriageProposal(f.service.Key, *proposal, f.principal, f.now))
			assert.Zero(t, f.writes.Load())
			receipt, err := f.archive.Store.LookupInboxReceipt(t.Context(), f.principal.ID, f.source.SourceID, "triage-item-1")
			require.NoError(t, err)
			assert.Nil(t, receipt)
		})
	}
}

func TestInboxGmailTriagePreviewRejectsChangedArchiveAndAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*triagePreviewFixture)
		want   error
	}{
		{"incoming", func(f *triagePreviewFixture) { f.onRead = func() { f.archive.CreateMessage("new-incoming") } }, inboxcontrol.ErrPlanChanged},
		{"mapping", func(f *triagePreviewFixture) {
			f.onRead = func() {
				_, err := f.archive.Store.ReplaceInboxTriageMappings(context.Background(), f.source, map[string]string{"todo": "WatchLabel"}, 1, inboxcontrol.Principal{ID: "owner-fixture", Owner: true})
				assert.NoError(t, err)
			}
		}, inboxcontrol.ErrPlanChanged},
		{"revoked", func(f *triagePreviewFixture) { f.onRead = func() { f.denied.Store(true) } }, inboxcontrol.ErrDenied},
		{"live read changed", func(f *triagePreviewFixture) { f.labels = []string{"INBOX", "STARRED", "Unrelated"} }, inboxcontrol.ErrPlanChanged},
		{"unknown category", func(f *triagePreviewFixture) { f.input.Items[0].Categories = []string{"autoarchive"} }, inboxcontrol.ErrInvalid},
		{"foreign evidence", func(f *triagePreviewFixture) {
			f.input.Items[0].EvidenceMessageIDs = []int64{f.archive.CreateMessage("other-message")}
		}, inboxcontrol.ErrDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTriagePreviewFixture(t)
			tc.change(f)
			p, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
			assert.ErrorIs(t, err, tc.want)
			assert.Nil(t, p)
			assert.Zero(t, f.writes.Load())
		})
	}
}

func TestInboxGmailTriageProposalAuthenticatesEveryReviewField(t *testing.T) {
	f := newTriagePreviewFixture(t)
	p, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
	require.NoError(t, err)
	encoded, err := json.Marshal(p)
	require.NoError(t, err)
	for _, mutate := range []func(*inboxcontrol.TriageProposal){
		func(p *inboxcontrol.TriageProposal) { p.MappingRevision++ },
		func(p *inboxcontrol.TriageProposal) { p.ArchiveRevision = strings.Repeat("a", 64) },
		func(p *inboxcontrol.TriageProposal) { p.IncomingWatermark = strings.Repeat("b", 64) },
		func(p *inboxcontrol.TriageProposal) { p.Items[0].Request.IdempotencyKey = "different-key" },
		func(p *inboxcontrol.TriageProposal) { p.Items[0].EvidenceMessageIDs = nil },
		func(p *inboxcontrol.TriageProposal) { p.Items[0].Classification = "finished" },
		func(p *inboxcontrol.TriageProposal) { p.Items[0].Request.Tags.Add = []string{"FinishedLabel"} },
		func(p *inboxcontrol.TriageProposal) { p.Items[0].RetainInbox = false },
		func(p *inboxcontrol.TriageProposal) { p.Items[0].Projected.Read = new(true) },
		func(p *inboxcontrol.TriageProposal) { p.ExpiresAt = p.ExpiresAt.Add(time.Minute) },
	} {
		var changed inboxcontrol.TriageProposal
		require.NoError(t, json.Unmarshal(encoded, &changed))
		mutate(&changed)
		assert.Error(t, inboxcontrol.VerifyTriageProposal(f.service.Key, changed, f.principal, f.now))
	}
	assert.Error(t, inboxcontrol.VerifyTriageProposal(f.service.Key, *p, inboxcontrol.Principal{ID: "other-delegate"}, f.now))
	assert.Error(t, inboxcontrol.VerifyTriageProposal(f.service.Key, *p, f.principal, p.ExpiresAt))
}

func TestInboxGmailTriagePreviewBoundsAndMissingMappings(t *testing.T) {
	f := newTriagePreviewFixture(t)
	for _, tc := range []struct {
		name   string
		change func(*inboxcontrol.TriageInput)
	}{
		{"empty", func(i *inboxcontrol.TriageInput) { i.Items = nil }},
		{"too many", func(i *inboxcontrol.TriageInput) { i.Items = make([]inboxcontrol.TriageItemInput, 101) }},
		{"duplicate target", func(i *inboxcontrol.TriageInput) {
			i.Items = append(i.Items, i.Items[0])
			i.Items[1].IdempotencyKey = "other-key"
		}},
		{"long key", func(i *inboxcontrol.TriageInput) { i.Items[0].IdempotencyKey = strings.Repeat("k", 129) }},
		{"duplicate categories", func(i *inboxcontrol.TriageInput) { i.Items[0].Categories = []string{"todo", "todo"} }},
		{"too much evidence", func(i *inboxcontrol.TriageInput) { i.Items[0].EvidenceMessageIDs = make([]int64, 101) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := f.input
			input.Items = slices.Clone(input.Items)
			tc.change(&input)
			p, err := f.service.PreviewTriage(t.Context(), input, f.principal)
			assert.Nil(t, p)
			assert.ErrorIs(t, err, inboxcontrol.ErrInvalid)
		})
	}
	_, err := f.archive.Store.DB().Exec(`DELETE FROM inbox_triage_mappings`)
	require.NoError(t, err)
	p, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
	assert.Nil(t, p)
	assert.ErrorIs(t, err, inboxcontrol.ErrDenied)
	assert.Zero(t, f.writes.Load())
}

func TestInboxGmailTriagePreviewAcceptsHundredExplicitTargets(t *testing.T) {
	f := newTriagePreviewFixture(t)
	f.input.Items = make([]inboxcontrol.TriageItemInput, 100)
	for i := range f.input.Items {
		target := inboxcontrol.Target{SourceID: f.source.SourceID, SourceType: "gmail", SourceIdentifier: f.source.SourceIdentifier, AccountID: f.source.AccountID, Scope: inboxcontrol.ScopeMessage, ProviderID: fmt.Sprintf("triage-message-%d", i)}
		target.ItemID = f.archive.CreateMessage(target.ProviderID)
		_, err := f.archive.Store.ObserveInboxState(t.Context(), inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), Tags: slices.Clone(f.labels), Revision: "12", ObservedAt: f.now})
		require.NoError(t, err)
		f.input.Items[i] = inboxcontrol.TriageItemInput{Target: target, Categories: []string{"todo"}, IdempotencyKey: fmt.Sprintf("triage-key-%d", i)}
	}
	p, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
	require.NoError(t, err)
	require.Len(t, p.Items, 100)
	for i, item := range p.Items {
		assert.Equal(t, f.input.Items[i].Target, *item.Request.Target)
		assert.Equal(t, f.input.Items[i].IdempotencyKey, item.Request.IdempotencyKey)
	}
	assert.NoError(t, inboxcontrol.VerifyTriageProposal(f.service.Key, *p, f.principal, f.now))
	assert.Zero(t, f.writes.Load())
}

func TestInboxGmailTriageExistingRetainedTagOverridesFinished(t *testing.T) {
	f := newTriagePreviewFixture(t)
	f.labels = append(f.labels, "TodoLabel")
	target := f.input.Items[0].Target
	_, err := f.archive.Store.ObserveInboxState(t.Context(), inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), Tags: slices.Clone(f.labels), Revision: "12", ObservedAt: f.now.Add(time.Second)})
	require.NoError(t, err)
	f.input.Items[0].Categories = []string{"finished"}
	p, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
	require.NoError(t, err)
	require.Len(t, p.Items, 1)
	assert.Equal(t, "todo", p.Items[0].Classification)
	assert.Equal(t, []string{"TodoLabel"}, p.Items[0].Request.Tags.Add)
	assert.NotContains(t, p.Items[0].Projected.Tags, "FinishedLabel")
	assert.True(t, p.Items[0].RetainInbox)
	assert.Zero(t, f.writes.Load())
}

func TestInboxGmailTriagePreviewGeneratesBoundedKeysWhenOmitted(t *testing.T) {
	f := newTriagePreviewFixture(t)
	f.input.Items[0].IdempotencyKey = ""
	first, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
	require.NoError(t, err)
	second, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
	require.NoError(t, err)
	require.Len(t, first.Items, 1)
	require.Len(t, second.Items, 1)
	assert.Len(t, first.Items[0].Request.IdempotencyKey, 32)
	assert.NotEqual(t, first.Items[0].Request.IdempotencyKey, second.Items[0].Request.IdempotencyKey)
	assert.NoError(t, inboxcontrol.VerifyTriageProposal(f.service.Key, *first, f.principal, f.now))
	assert.Zero(t, f.writes.Load())
}

func TestInboxGmailTriageProposalCannotBypassApplyGuardsViaControl(t *testing.T) {
	f := newTriagePreviewFixture(t)
	p, err := f.service.PreviewTriage(t.Context(), f.input, f.principal)
	require.NoError(t, err)
	require.Len(t, p.Items, 1)
	assert.Empty(t, p.Items[0].Request.PreviewToken, "only the whole triage proposal may authorize its apply path")
	result, err := f.service.Control(t.Context(), p.Items[0].Request, f.principal, nil)
	assert.Nil(t, result)
	assert.ErrorIs(t, err, inboxcontrol.ErrInvalid)
	assert.Zero(t, f.writes.Load())
}
