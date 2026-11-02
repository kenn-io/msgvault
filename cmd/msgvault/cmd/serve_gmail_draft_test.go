package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

func TestAuthorizeGmailDraftRequiresEnabledSourcePolicy(t *testing.T) {
	requirements := require.New(t)
	requirements.ErrorContains(authorizeGmailDraft(nil, 42, "gmail"), "draft_disabled")
	requirements.ErrorContains(authorizeGmailDraft([]config.GmailDraftSource{{SourceID: 42}}, 42, "gmail"), "draft_disabled")
	requirements.NoError(authorizeGmailDraft([]config.GmailDraftSource{{SourceID: 42, Enabled: true}}, 42, "gmail"))
	requirements.ErrorContains(authorizeGmailDraft([]config.GmailDraftSource{{SourceID: 42, Enabled: true}}, 42, "imap"), "draft_disabled")
}

func TestValidateGmailSendAsRequiresPrimaryOrAccepted(t *testing.T) {
	requirements := require.New(t)
	entries := []gmail.SendAs{
		{Email: "pending@example.com", VerificationStatus: "pending"},
		{Email: "accepted@example.com", VerificationStatus: "accepted"},
		{Email: "primary@example.com", Primary: true, VerificationStatus: "pending"},
	}
	requirements.NoError(validateGmailSendAs(entries, "ACCEPTED@example.com"))
	requirements.NoError(validateGmailSendAs(entries, "primary@example.com"))
	requirements.ErrorContains(validateGmailSendAs(entries, "pending@example.com"), "invalid_from")
	requirements.ErrorContains(validateGmailSendAs(entries, "missing@example.com"), "invalid_from")
}

func TestParseDraftSendAsArgs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	account, jsonOutput, err := parseDraftSendAsArgs([]string{"draft-send-as", "alice@example.com", "--json"})
	requirements.NoError(err)
	assertions.Equal("alice@example.com", account)
	assertions.True(jsonOutput)
	_, _, err = parseDraftSendAsArgs([]string{"draft-send-as"})
	requirements.ErrorContains(err, "invalid_args")
}

func TestGmailDraftMessagePersistDataUsesHTMLForDerivedText(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	parsed := &msgmime.Message{
		From:     []msgmime.Address{{Email: "sender@example.test"}},
		Subject:  "Synthetic draft",
		BodyHTML: `<p>HTML-only draft content.</p>`,
	}
	build := gmailDraftMessagePersistData(
		7, 0, parsed, []byte("synthetic mime"),
		store.GmailDraftReceipt{GmailMessageID: "draft-message", ThreadID: "draft-thread"}, "",
	)
	data := build([]int64{11})
	requirements.NotNil(data)
	checks.True(data.BodyText.Valid)
	checks.Empty(data.BodyText.String, "canonical plain text remains empty")
	checks.True(data.BodyHTML.Valid)
	checks.Equal(parsed.BodyHTML, data.BodyHTML.String)
	checks.True(data.Message.Snippet.Valid)
	checks.Equal("HTML-only draft content.", data.Message.Snippet.String)
	requirements.NotNil(data.FTS)
	checks.Equal("HTML-only draft content.", data.FTS.Body)
}

func TestGmailReadErrorCodeUsesProviderMessages(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "access token scope insufficient",
			err:  errors.New("googleapi: Error 403: ACCESS_TOKEN_SCOPE_INSUFFICIENT"),
			want: "insufficient_scope",
		},
		{
			name: "insufficient authentication scopes",
			err:  errors.New("googleapi: Error 403: insufficient authentication scopes"),
			want: "insufficient_scope",
		},
		{
			name: "insufficient permission",
			err:  errors.New("googleapi: Error 403: Insufficient Permission"),
			want: "insufficient_scope",
		},
		{
			name: "generic insufficient text",
			err:  errors.New("provider returned insufficient data"),
			want: "provider_refused",
		},
		{
			name: "draft not found",
			err:  &gmail.NotFoundError{Path: "/drafts/draft-1"},
			want: "provider_absent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, gmailReadErrorCode(tt.err))
		})
	}
}

func TestEmitGmailDraftLifecycleHumanPendingOutput(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	output := gmailDraftLifecycleOutput{
		Status:           "pending",
		Provider:         "gmail",
		DraftID:          "draft-1",
		Revision:         2,
		Lifecycle:        "active",
		Receipt:          gmailDraftLifecycleReceipt{GmailDraftID: "gmail-draft-1", GmailMessageID: "gmail-message-1", ThreadID: "thread-1"},
		Content:          "current content\n",
		CandidateContent: "candidate content\n",
		PendingOperation: "edit",
		PendingCode:      "remote_unknown",
		Observation: &gmailDraftLifecycleObservation{
			State:          "unknown",
			Code:           "remote_unknown",
			GmailDraftID:   "gmail-draft-1",
			GmailMessageID: "gmail-message-1",
			ThreadID:       "thread-1",
			Present:        true,
		},
	}

	var event api.CLIRunEvent
	err := emitGmailDraftLifecycleOutput(func(got api.CLIRunEvent) error {
		event = got
		return nil
	}, cliStreamStderr, draftLifecycleIntent{}, output)
	requirements.NoError(err)
	assertions.Equal(cliStreamStderr, event.Type)
	assertions.Contains(event.Data, "receipt (revision 2): gmail_draft_id=gmail-draft-1")
	assertions.Contains(event.Data, "pending operation: edit")
	assertions.Contains(event.Data, "candidate content:\ncandidate content")
	assertions.Contains(event.Data, "old provider receipt: state=unknown code=remote_unknown")
	assertions.Contains(event.Data, "provider outcome: remote_unknown")
	assertions.Contains(event.Data, "retry draft-edit with --revision 2 to reconcile with Gmail")
	assertions.Equal(1, strings.Count(event.Data, "provider outcome:"))
}
