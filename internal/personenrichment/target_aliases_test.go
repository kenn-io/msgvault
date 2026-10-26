package personenrichment_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestPeopleTargetAliases(t *testing.T) {
	for _, kind := range []string{personenrichment.ProviderContext, personenrichment.ProviderExa} {
		t.Run(kind, func(t *testing.T) {
			body := []byte(`{"request_id":"r","results":[` + contextPerson + `]}`)
			name := "Alex Example"
			if kind == personenrichment.ProviderExa {
				body = exaFixture(t, "exa_people_success.json")
				name = "Test User"
			}
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer server.Close()
			var provider personenrichment.Provider
			var err error
			if kind == personenrichment.ProviderContext {
				provider, err = personenrichment.NewContextProvider(contextConfig(server.URL), "test-key", server.Client())
			} else {
				provider, err = personenrichment.NewExaProvider(exaConfig(server.URL, "people", 1), "test-key", server.Client())
			}
			require.NoError(t, err)
			for _, test := range []struct {
				alias       string
				cardinality personfacts.Cardinality
				contextOK   bool
				exaOK       bool
			}{
				{"name", personfacts.CardinalitySingle, true, true},
				{"full_name", personfacts.CardinalitySingle, true, true},
				{"first_name", personfacts.CardinalitySingle, false, true},
				{"last_name", personfacts.CardinalitySingle, false, true},
				{"location", personfacts.CardinalitySingle, true, true},
				{"education", personfacts.CardinalityMulti, true, true},
				{"education_history", personfacts.CardinalityMulti, true, true},
				{"summary", personfacts.CardinalitySingle, false, false},
				{"name", personfacts.CardinalityMulti, false, true},
				{"location", personfacts.CardinalityMulti, false, true},
				{"education_history", personfacts.CardinalitySingle, false, true},
			} {
				t.Run(test.alias+"/"+string(test.cardinality), func(t *testing.T) {
					target := exaAttributeTarget(t, "attribute:"+test.alias, personfacts.ValueText, test.cardinality)
					before := calls
					attempt, err := provider.Start(t.Context(), personenrichment.Request{
						Identity: personenrichment.Identity{Name: name, CurrentCompany: "Example Labs"},
						Targets:  []personfacts.TargetDescriptor{target},
					})
					ok := test.exaOK
					if kind == personenrichment.ProviderContext {
						ok = test.contextOK
					}
					if !ok {
						require.Error(t, err)
						assert.Equal(t, before, calls)
						return
					}
					require.NoError(t, err)
					require.NotNil(t, attempt.Result)
					require.Len(t, attempt.Result.Claims, 1)
					assert.Equal(t, target, attempt.Result.Claims[0].Target)
					switch test.alias {
					case "name", "full_name":
						assert.JSONEq(t, `"`+name+`"`, string(attempt.Result.Claims[0].SubmittedValue))
					case "education", "education_history":
						want := `"Example University"`
						if kind == personenrichment.ProviderExa {
							want = `"BS Computing — Example University"`
						}
						assert.JSONEq(t, want, string(attempt.Result.Claims[0].SubmittedValue))
					}
				})
			}
		})
	}
}
