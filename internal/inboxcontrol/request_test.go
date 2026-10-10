package inboxcontrol

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/emailtags"
)

func TestRequestRequiresBoundedIntent(t *testing.T) {
	target := Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: ScopeMessage, ItemID: 2, ProviderID: "synthetic-message"}
	expected := State{Target: target, ObservedAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)}
	good := Request{Operation: OpArchive, Target: &target, Expected: &expected, PreviewToken: "signed-preview", IdempotencyKey: "synthetic-operation"}
	require.NoError(t, good.Validate())
	for _, tc := range []struct {
		name   string
		change func(*Request)
	}{
		{"missing target", func(v *Request) { v.Target = nil }},
		{"unbounded query operation", func(v *Request) { v.Operation = "archive-query" }},
		{"missing preview", func(v *Request) { v.PreviewToken = "" }},
		{"missing state", func(v *Request) { v.Expected = nil }},
		{"missing key", func(v *Request) { v.IdempotencyKey = "" }},
		{"oversize key", func(v *Request) { v.IdempotencyKey = strings.Repeat("a", 129) }},
		{"invalid UTF8 key", func(v *Request) { v.IdempotencyKey = "\xff" }},
		{"different account", func(v *Request) {
			changed := expected
			changed.Target.AccountID = "other@example.com"
			v.Expected = &changed
		}},
		{"tag payload on archive", func(v *Request) { v.Tags = &emailtags.Change{Add: []string{"Todo"}} }},
		{"destination on archive", func(v *Request) { v.Destination = &Folder{ID: "user-label"} }},
		{"receipt on archive", func(v *Request) { v.ReceiptID = "receipt" }},
	} {
		t.Run(tc.name, func(t *testing.T) { r := good; tc.change(&r); assert.ErrorIs(t, r.Validate(), ErrInvalid) })
	}
}

func TestRequestPreviewAndReadNeedNoExistingPlan(t *testing.T) {
	target := Target{SourceID: 1, SourceType: "beeper", SourceIdentifier: "synthetic-account", AccountID: "synthetic-account", Scope: ScopeChat, ItemID: 2, ProviderID: "!synthetic:example.com"}
	require.NoError(t, (Request{Operation: OpArchive, Target: &target, DryRun: true}).Validate())
	require.NoError(t, (Request{Operation: OpGetState, Target: &target}).Validate())
	assert.ErrorIs(t, (Request{Operation: OpGetState, Target: &target, IdempotencyKey: "operation"}).Validate(), ErrInvalid)
}

func TestRequestPreviewRejectsExecutionEvidence(t *testing.T) {
	target := Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: ScopeMessage, ItemID: 2, ProviderID: "provider-message"}
	for _, tc := range []struct {
		name   string
		change func(*Request)
	}{
		{"expected", func(r *Request) { r.Expected = &State{Target: target} }},
		{"preview token", func(r *Request) { r.PreviewToken = "previous-preview" }},
		{"idempotency key", func(r *Request) { r.IdempotencyKey = "previous-key" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Request{Operation: OpArchive, Target: &target, DryRun: true}
			tc.change(&r)
			assert.ErrorIs(t, r.Validate(), ErrInvalid)
		})
	}
}

func TestRequestSeparatesTagsAndRead(t *testing.T) {
	target := Target{SourceID: 1, SourceType: "imap", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: ScopeMessage, ItemID: 2, ProviderID: "INBOX|3", Mailbox: "INBOX", UIDValidity: 42, UID: 3}
	good := Request{Operation: OpTags, Target: &target, DryRun: true, Tags: &emailtags.Change{Add: []string{"Todo"}}}
	require.NoError(t, good.Validate())
	for _, change := range []func(*Request){
		func(v *Request) { v.Operation = OpSetRead },
		func(v *Request) { v.Tags = &emailtags.Change{} },
		func(v *Request) { v.Tags = &emailtags.Change{Add: []string{"Todo"}, Remove: []string{"todo"}} },
		func(v *Request) { v.Tags = &emailtags.Change{Add: []string{"Todo"}, Mailbox: "Elsewhere"} },
		func(v *Request) { v.Tags = &emailtags.Change{Add: []string{"Todo"}, DryRun: true} },
	} {
		r := good
		change(&r)
		assert.ErrorIs(t, r.Validate(), ErrInvalid)
	}
}

func TestRequestSourceOperationsHaveNoMessageTarget(t *testing.T) {
	requirements := require.New(t)

	source := SourceIdentity{SourceID: 1, SourceType: "imap", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com"}
	requirements.NoError((Request{Operation: OpListFolders, Source: &source}).Validate())
	requirements.NoError((Request{Operation: OpCreateFolder, Source: &source, Destination: &Folder{Name: "Archive"}, DryRun: true}).Validate())
	requirements.ErrorIs((Request{Operation: OpListFolders}).Validate(), ErrInvalid)
	requirements.ErrorIs((Request{Operation: OpCreateFolder, Source: &source, DryRun: true}).Validate(), ErrInvalid)
	target := Target{SourceID: 1, SourceType: "imap", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: ScopeMessage, ItemID: 2, ProviderID: "INBOX|3", Mailbox: "INBOX", UIDValidity: 42, UID: 3}
	requirements.ErrorIs((Request{Operation: OpListFolders, Source: &source, Target: &target}).Validate(), ErrInvalid)
	requirements.ErrorIs((Request{Operation: OpMove, Target: &target, DryRun: true}).Validate(), ErrInvalid)
	requirements.NoError((Request{Operation: OpMove, Target: &target, Destination: &Folder{ID: "Archive"}, DryRun: true}).Validate())
}

func TestRequestReceiptRecoveryIsExplicit(t *testing.T) {
	require.NoError(t, (Request{Operation: OpReceiptGet, ReceiptID: "synthetic-receipt"}).Validate())
	require.NoError(t, (Request{Operation: OpReconcile, ReceiptID: "synthetic-receipt"}).Validate())
	assert.ErrorIs(t, (Request{Operation: OpReconcile}).Validate(), ErrInvalid)
}

func TestOperationRequiresSpecificGrant(t *testing.T) {
	assertions := assert.New(t)

	for _, tc := range []struct {
		op         Operation
		permission agentgrant.Permission
	}{
		{OpGetState, agentgrant.PermissionInboxRead},
		{OpListFolders, agentgrant.PermissionInboxRead},
		{OpTags, agentgrant.PermissionInboxTag},
		{OpArchive, agentgrant.PermissionInboxArchive},
		{OpUnarchive, agentgrant.PermissionInboxArchive},
		{OpSetRead, agentgrant.PermissionInboxReadState},
		{OpSetUnread, agentgrant.PermissionInboxReadState},
		{OpMove, agentgrant.PermissionInboxMove},
		{OpCreateFolder, agentgrant.PermissionInboxFolderCreate},
	} {
		permission, err := tc.op.RequiredPermission()
		require.NoError(t, err)
		assertions.Equal(tc.permission, permission)
		known, ok := agentgrant.KnownPermission(string(permission))
		assertions.True(ok)
		assertions.Equal(permission, known)
	}
	_, err := Operation("archive-query").RequiredPermission()
	require.ErrorIs(t, err, ErrInvalid)
	grant := agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionInboxArchive}, Sources: []agentgrant.SourceRef{{ID: 1, Type: "gmail", Identifier: "reader@example.com"}}}
	assertions.True(grant.Allows(agentgrant.PermissionInboxArchive, agentgrant.SourceRef{Type: "gmail", Identifier: "reader@example.com"}))
	assertions.False(grant.Allows(agentgrant.PermissionInboxArchive, agentgrant.SourceRef{Type: "gmail", Identifier: "other@example.com"}))
	assertions.False(grant.Allows(agentgrant.PermissionInboxReadState, agentgrant.SourceRef{Type: "gmail", Identifier: "reader@example.com"}))
}

func TestRequestGmailMoveRequiresExplicitOrigin(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	target := Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.com", AccountID: "owner@example.com", Scope: ScopeMessage, ItemID: 1, ProviderID: "message-1"}
	request := Request{Operation: OpMove, Target: &target, Destination: &Folder{ID: "LabelNew"}, DryRun: true}
	requirements.ErrorIs(request.Validate(), ErrInvalid)
	request.OriginFolder = &Folder{ID: "LabelOld"}
	requirements.NoError(request.Validate())
	for _, origin := range []*Folder{{ID: "LabelNew"}, {Name: "Old"}, {ID: "LabelOld", UIDValidity: 7}} {
		changedRequest := request
		changedRequest.OriginFolder = origin
		require.ErrorIs(t, changedRequest.Validate(), ErrInvalid)
	}
	request.Operation = OpArchive
	request.Destination = nil
	assertions.ErrorIs(request.Validate(), ErrInvalid)
}

func TestRequestIMAPUnarchiveIsBoundToInboxEpoch(t *testing.T) {
	target := Target{SourceID: 1, SourceType: "imap", SourceIdentifier: "owner@example.test", AccountID: "owner@example.test", Scope: ScopeMessage, ItemID: 2, ProviderID: "Archive|42", Mailbox: "Archive", UIDValidity: 9, UID: 42}
	valid := Request{Operation: OpUnarchive, Target: &target, Destination: &Folder{ID: "INBOX", UIDValidity: 17}, DryRun: true}
	require.NoError(t, valid.Validate())

	for _, destination := range []*Folder{
		{ID: "Trash", UIDValidity: 17},
		{ID: "INBOX"},
		{ID: "INBOX", UIDValidity: 0},
	} {
		request := valid
		request.Destination = destination
		assert.ErrorIs(t, request.Validate(), ErrInvalid)
	}
}
