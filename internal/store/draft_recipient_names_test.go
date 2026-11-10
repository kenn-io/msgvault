package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			history := func(id int64, key string) int64 {
				msg := f.CreateMessage(key)
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET sender_id = ? WHERE id = ?`), id, msg)
				require.NoError(t, err)
				return msg
			}
			switch scenario {
			case "curated names and source scope":
				id := seed("alex@example.com", "Alex", "Al", "Jordan Rivera")
				got, err := f.Store.CuratedNamesForAddressesContext(t.Context(), []string{"ALEX@example.com", "pat@example.com"}, nil)
				require.NoError(t, err)
				assert.ElementsMatch(t, []string{"Alex", "Al", "Jordan"}, got["alex@example.com"])
				assert.NotContains(t, got, "pat@example.com")
				for _, scope := range [][]int64{{}, {f.Source.ID + 100}} {
					got, err = f.Store.CuratedNamesForAddressesContext(t.Context(), []string{"alex@example.com"}, scope)
					require.NoError(t, err)
					assert.Empty(t, got)
				}
				history(id, "history")
				got, err = f.Store.CuratedNamesForAddressesContext(t.Context(), []string{"alex@example.com"}, []int64{f.Source.ID})
				require.NoError(t, err)
				assert.ElementsMatch(t, []string{"Alex", "Al", "Jordan"}, got["alex@example.com"])
			case "ambiguous alias":
				alex := seed("alex@example.com", "Alex", "", "")
				pat := seed("pat@example.com", "Pat", "", "")
				require.NoError(t, f.Store.SetParticipantIdentifierContext(t.Context(), pat, "email", "alex@example.com"))
				history(alex, "history-Alex")
				otherMessage := history(pat, "history-Pat")
				for _, scope := range [][]int64{nil, {f.Source.ID}} {
					got, err := f.Store.CuratedNamesForAddressesContext(t.Context(), []string{"alex@example.com"}, scope)
					require.NoError(t, err)
					assert.Empty(t, got)
				}
				other, err := f.Store.GetOrCreateSource("imap", "other@example.com")
				require.NoError(t, err)
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_id = ? WHERE id = ?`), other.ID, otherMessage)
				require.NoError(t, err)
				got, err := f.Store.CuratedNamesForAddressesContext(t.Context(), []string{"alex@example.com"}, []int64{f.Source.ID})
				require.NoError(t, err)
				assert.Equal(t, []string{"Alex"}, got["alex@example.com"])
			case "quoted mailbox":
				seed("alex smith@example.com", "Alex", "", "")
				got, err := f.Store.CuratedNamesForAddressesContext(t.Context(), []string{`"alex smith"@example.com`}, nil)
				require.NoError(t, err)
				assert.Equal(t, []string{"Alex"}, got["alex smith@example.com"])
			}
		})
	}
}
