package carddav

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestInferredNamePublicationReviewMatchesRenderedCard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    store.PersonNameInput
		wantWire string
	}{
		{
			name: "sort",
			input: store.PersonNameInput{
				NameKind: store.PersonNameSort, SortAs: new("Example, Alice"),
			},
		},
		{
			name: "formatted original only",
			input: store.PersonNameInput{
				NameKind: store.PersonNameFormatted, OriginalValue: "Alice Example",
			},
		},
		{
			name: "blank formatted value",
			input: store.PersonNameInput{
				NameKind: store.PersonNameFormatted, Formatted: new(" \t "), OriginalValue: "Alice Example",
			},
		},
		{
			name: "structured original only",
			input: store.PersonNameInput{
				NameKind: store.PersonNameStructured, OriginalValue: "Example;Alice;;;",
			},
		},
		{
			name: "blank structured components",
			input: store.PersonNameInput{
				NameKind: store.PersonNameStructured, GivenName: new(" \t "), OriginalValue: "Example;Alice;;;",
			},
		},
		{
			name: "phonetic original only",
			input: store.PersonNameInput{
				NameKind: store.PersonNamePhonetic, OriginalValue: "Alis",
			},
		},
		{
			name: "rendered nickname original",
			input: store.PersonNameInput{
				NameKind: store.PersonNameNickname, Formatted: new(" \t "), OriginalValue: "Ally",
			},
			wantWire: "NICKNAME:Ally",
		},
		{
			name: "rendered structured name",
			input: store.PersonNameInput{
				NameKind: store.PersonNameStructured, FamilyName: new("Example"), GivenName: new("Alice"),
			},
			wantWire: "N:Example;Alice;;;",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := &mutationFixture{}
			service, st, id, _ := seededMutationService(t, fixture)
			appendInferenceReviewNote(t, st, id, "Reviewed note")
			preview, err := service.PreviewPublication(t.Context(), id)
			require.NoError(err)
			require.NoError(service.PublishReviewedPerson(t.Context(), id, preview.ApprovalToken))

			tc.input.Envelope.Source = store.ProvenanceEnrichment
			_, err = st.AddPersonNameContext(t.Context(), id, tc.input)
			require.NoError(err)
			after, err := service.PreviewPublication(t.Context(), id)
			require.NoError(err)
			if tc.wantWire == "" {
				assert.Equal(preview.VCard, after.VCard)
				assert.False(after.ReviewRequired)
				require.NoError(service.PublishPerson(t.Context(), id))
				_, err = service.Sync(t.Context(), SyncOptions{Full: true})
				require.NoError(err)
				view, err := service.PublicationView(t.Context(), id)
				require.NoError(err)
				assert.False(view.InferenceReviewRequired)
			} else {
				assert.Contains(after.VCard, tc.wantWire)
				assert.NotEqual(preview.VCard, after.VCard)
				assert.True(after.ReviewRequired)
				require.ErrorIs(service.PublishPerson(t.Context(), id), store.ErrCardDAVInferenceReviewRequired)
			}
			fixture.mu.Lock()
			assert.Equal(1, fixture.puts)
			fixture.mu.Unlock()
		})
	}
}
