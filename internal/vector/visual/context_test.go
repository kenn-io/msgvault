package visual

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestAssembleDocumentOrdersBoundedContextThenMedia(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	media := &MediaInput{Kind: MediaKindImage, MIMEType: "image/png", BlobHash: testHash, Bytes: []byte("png"), Width: 2, Height: 2}
	doc, truncated, err := AssembleDocument(AssembleRequest{
		Owner: Owner{MessageID: 9, BlobHash: testHash, MediaInputKey: OriginalMediaInputKey},
		Media: media,
		Context: MessageContext{
			Subject: "Launch", Body: "<p>photo caption</p>\n\n> quoted history\n-- \nsignature",
			MessageType: "slack", Filename: "secret-name.png", ThreadHistory: "other message",
		},
		Role: store.AttachmentRoleStandalone, SourceSequence: 44,
		Policy: ContextPolicy{MaxChars: 60, InputVersion: "visual-input-v1", EligibilityVersion: "visual-eligibility-v1"},
	})
	require.NoError(err)
	assert.False(truncated)
	require.Len(doc.Parts, 2)
	assert.Equal("Subject: Launch\n\nphoto caption\n\nMessage type: chat message", doc.Parts[0].Text)
	assert.Same(media, doc.Parts[1].Media)
	assert.NotContains(doc.Parts[0].Text, "secret-name")
	assert.NotContains(doc.Parts[0].Text, "other message")
	assert.Equal(int64(44), doc.SourceSequence)
}

func TestAssembleDocumentRevisionIgnoresFenceAndOperationalMetadata(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	request := revisionRequest()
	first, _, err := AssembleDocument(request)
	require.NoError(err)

	request.SourceSequence++
	request.Context.Labels = []string{"read", "important"}
	request.Context.Filename = "renamed.png"
	request.Context.ThreadHistory = "new surrounding chat"
	second, _, err := AssembleDocument(request)
	require.NoError(err)
	assert.Equal(first.Revision, second.Revision)

	mutations := []func(*AssembleRequest){
		func(r *AssembleRequest) { r.Context.Body = "changed caption" },
		func(r *AssembleRequest) { r.Context.Subject = "changed subject" },
		func(r *AssembleRequest) { r.Role = store.AttachmentRoleInline },
		func(r *AssembleRequest) {
			r.Media.BlobHash = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
			r.Owner.BlobHash = r.Media.BlobHash
		},
		func(r *AssembleRequest) { r.Policy.InputVersion = "visual-input-v2" },
	}
	for i, mutate := range mutations {
		changed := revisionRequest()
		mutate(&changed)
		doc, _, assembleErr := AssembleDocument(changed)
		require.NoError(assembleErr)
		assert.NotEqual(first.Revision, doc.Revision, "mutation %d", i)
	}
}

func TestAssembleDocumentUsesMediaOnlyWhenContextNormalizesEmpty(t *testing.T) {
	request := revisionRequest()
	request.Context = MessageContext{Body: "   \n\n "}
	doc, _, err := AssembleDocument(request)
	require.NoError(t, err)
	require.Len(t, doc.Parts, 1)
	assert.Same(t, request.Media, doc.Parts[0].Media)
}

func TestAssembleDocumentTruncatesContextOnRuneBoundary(t *testing.T) {
	request := revisionRequest()
	request.Context = MessageContext{Body: "αβγδε"}
	request.Policy.MaxChars = 3
	doc, truncated, err := AssembleDocument(request)
	require.NoError(t, err)
	require.True(t, truncated)
	assert.Equal(t, "αβγ", doc.Parts[0].Text)
}

func revisionRequest() AssembleRequest {
	return AssembleRequest{
		Owner:   Owner{MessageID: 9, BlobHash: testHash, MediaInputKey: OriginalMediaInputKey},
		Media:   &MediaInput{Kind: MediaKindImage, MIMEType: "image/png", BlobHash: testHash, Bytes: []byte("png"), Width: 2, Height: 2},
		Context: MessageContext{Subject: "subject", Body: "caption", MessageType: "email"},
		Role:    store.AttachmentRoleStandalone, SourceSequence: 1,
		Policy: ContextPolicy{MaxChars: 4000, InputVersion: "visual-input-v1", EligibilityVersion: "visual-eligibility-v1"},
	}
}
