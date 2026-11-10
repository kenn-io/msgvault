package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/draftlint"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCuratedNamesForAddresses(t *testing.T) {
	for _, scenario := range []string{"curated names and source scope", "ambiguous alias", "quoted mailbox"} {
		t.Run(scenario, func(t *testing.T) {
			f := storetest.New(t)
			seed := func(address, given, nick, formatted string) int64 {
				id := f.EnsureParticipant(address, "Synthetic", "example.com")
				person, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), id)
				require.NoError(t, err)
				_, err = f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: store.PersonNameStructured, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}, GivenName: &given})
				require.NoError(t, err)
				for _, name := range []struct {
					kind  store.PersonNameKind
					value string
				}{{store.PersonNameNickname, nick}, {store.PersonNameFormatted, formatted}} {
					if name.value == "" {
						continue
					}
					_, err = f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: name.kind, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}, Formatted: &name.value})
					require.NoError(t, err)
				}
				return id
			}
			history := func(address, key string) int64 {
				return f.PersistEmail(storetest.EmailSpec{Thread: key, From: address, To: []string{storetest.OwnerAddress}, Body: "Historical message"})
			}
			switch scenario {
			case "curated names and source scope":
				id := seed("alex@example.com", "Alex", "Al", "Jordan Rivera")
				person, err := f.Store.PersonForParticipantsContext(t.Context(), []int64{id})
				require.NoError(t, err)
				require.NotNil(t, person)
				for _, kind := range []store.PersonNameKind{store.PersonNamePhonetic, store.PersonNameSort} {
					given := "Ineligible"
					_, err := f.Store.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: kind, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}, GivenName: &given})
					require.NoError(t, err)
				}
				got, err := f.Store.CuratedNamesForAddressesContext(t.Context(), []string{"ALEX@example.com", "pat@example.com"}, nil)
				require.NoError(t, err)
				assert.ElementsMatch(t, []draftlint.Name{{Text: "Alex", Given: true}, {Text: "Al"}, {Text: "Jordan"}}, got)
				history("alex@example.com", "history")
				got, err = f.Store.CuratedNamesForAddressesContext(t.Context(), []string{"alex@example.com"}, []int64{f.Source.ID + 100})
				require.NoError(t, err)
				assert.Empty(t, got)
				got, err = f.Store.CuratedNamesForAddressesContext(t.Context(), []string{"alex@example.com"}, []int64{f.Source.ID})
				require.NoError(t, err)
				assert.ElementsMatch(t, []draftlint.Name{{Text: "Alex", Given: true}, {Text: "Al"}, {Text: "Jordan"}}, got)
			case "ambiguous alias":
				seed("alex@example.com", "Alex", "", "")
				pat := seed("pat@example.com", "Pat", "", "")
				require.NoError(t, f.Store.SetParticipantIdentifierContext(t.Context(), pat, "email", "alex@example.com"))
				got, err := f.Store.CuratedNamesForAddressesContext(t.Context(), []string{"alex@example.com"}, nil)
				require.NoError(t, err)
				assert.Empty(t, got)
			case "quoted mailbox":
				seed("alex smith@example.com", "Alex", "", "")
				got, err := f.Store.CuratedNamesForAddressesContext(t.Context(), []string{`"alex smith"@example.com`}, nil)
				require.NoError(t, err)
				assert.Equal(t, []draftlint.Name{{Text: "Alex", Given: true}}, got)
			}
		})
	}
}
