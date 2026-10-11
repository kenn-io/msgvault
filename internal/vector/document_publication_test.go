package vector

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func publicationDoc(key string, sequence int64, members ...int64) DocumentPublication {
	return DocumentPublication{Key: key, Kind: "chat-window", Revision: "r1", SourceSequence: sequence, Members: members}
}

func publicationScope(key string, docs []DocumentPublication, chunkIDs ...int64) DocumentScopePublication {
	chunks := make([]Chunk, 0, len(chunkIDs))
	for _, id := range chunkIDs {
		chunks = append(chunks, Chunk{MessageID: id, Vector: []float32{1}})
	}
	return DocumentScopePublication{ScopeKey: key, SourceSequence: 1, Documents: docs, Chunks: chunks}
}

func TestValidateScopePublicationsRejections(t *testing.T) {
	preserved := publicationDoc("kept", 1, 7)
	preserved.PreserveVectors = true
	fenceOnly := publicationScope("s", []DocumentPublication{publicationDoc("d", 1, 7)}, 7)
	fenceOnly.FenceOnly = true
	missingKind := publicationDoc("d", 1, 7)
	missingKind.Kind = ""
	tests := []struct {
		name   string
		scopes []DocumentScopePublication
		want   string
	}{
		{"empty scope key", []DocumentScopePublication{publicationScope("", nil)},
			"publish scope: empty scope key"},
		{"duplicate scope key", []DocumentScopePublication{publicationScope("s", nil), publicationScope("s", nil)},
			`publish scopes: duplicate scope key "s"`},
		{"sequence mismatch", []DocumentScopePublication{publicationScope("s", []DocumentPublication{publicationDoc("d", 2, 7)}, 7)},
			`publish scope: document "d" source sequence 2 does not match scope sequence 1`},
		{"missing kind", []DocumentScopePublication{publicationScope("s", []DocumentPublication{missingKind}, 7)},
			"publish scope: document key, kind, and revision are required"},
		{"duplicate document key", []DocumentScopePublication{publicationScope("s", []DocumentPublication{publicationDoc("d", 1, 7), publicationDoc("d", 1, 8)}, 7, 8)},
			`publish scope: duplicate document key "d"`},
		{"member in two documents", []DocumentScopePublication{publicationScope("s", []DocumentPublication{publicationDoc("a", 1, 7), publicationDoc("b", 1, 7)}, 7)},
			`publish scope: message 7 belongs to both "a" and "b"`},
		{"fence-only with chunks", []DocumentScopePublication{fenceOnly},
			"publish scope: fence-only publication cannot contain chunks"},
		{"chunk without owner", []DocumentScopePublication{publicationScope("s", []DocumentPublication{publicationDoc("d", 1, 7)}, 7, 8)},
			"publish scope: chunk message 8 has no desired document owner"},
		{"preserved member with chunk", []DocumentScopePublication{publicationScope("s", []DocumentPublication{preserved}, 7)},
			"publish scope: preserved document member 7 also has a replacement chunk"},
		{"member without chunk", []DocumentScopePublication{publicationScope("s", []DocumentPublication{publicationDoc("d", 1, 7)})},
			"publish scope: document member 7 has no chunk"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validated, err := ValidateScopePublications(tt.scopes)
			require.Error(t, err)
			assert.Equal(t, tt.want, err.Error())
			assert.Nil(t, validated)
		})
	}

	t.Run("valid batch", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		validated, err := ValidateScopePublications([]DocumentScopePublication{
			publicationScope("s", []DocumentPublication{publicationDoc("b", 1, 8), preserved, publicationDoc("a", 1)}, 8),
		})
		require.NoError(err)
		require.Len(validated, 1)
		assert.Equal([]string{"a", "b", "kept"}, validated[0].DesiredKeys)
		assert.Equal(map[int64]string{7: "kept", 8: "b"}, validated[0].DocByMember)
	})
}

func TestPreservedDocumentMembersRejectsChangedRecord(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	desired := publicationDoc("d", 1, 7, 8)
	desired.PreserveVectors = true
	current := []DocumentRecord{{Key: "d", Kind: "chat-window", PublishedRevision: "r1", Members: []int64{7, 8}}}

	preserved, err := PreservedDocumentMembers(current, []DocumentPublication{desired, publicationDoc("other", 1, 9)})
	require.NoError(err)
	assert.Equal(map[int64]struct{}{7: {}, 8: {}}, preserved)

	current[0].PublishedRevision = "r0"
	_, err = PreservedDocumentMembers(current, []DocumentPublication{desired})
	require.ErrorIs(err, ErrDocumentFenceChanged)
	assert.EqualError(err, `document scope changed before sequence fence: preserved document "d" changed`)
}

func TestSameFenceDocuments(t *testing.T) {
	assert := assert.New(t)
	current := []DocumentRecord{{Key: "d", Kind: "chat-window", PublishedRevision: "r1", Members: []int64{7, 8}}}
	assert.True(SameFenceDocuments(current, []DocumentPublication{publicationDoc("d", 1, 7, 8)}))
	assert.False(SameFenceDocuments(current, []DocumentPublication{publicationDoc("d", 1, 8, 7)}), "reordered members")
	extra := []DocumentRecord{current[0], {Key: "e", Kind: "chat-window", PublishedRevision: "r1", Members: []int64{}}}
	assert.False(SameFenceDocuments(extra, []DocumentPublication{publicationDoc("d", 1, 7, 8)}), "extra record")
}
