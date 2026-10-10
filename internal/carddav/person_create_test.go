package carddav

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCreateStandalonePersonAndPublish(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := &mutationFixture{}
	service, st, _, _ := seededMutationService(t, fixture)
	person, err := st.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Example", Emails: []store.PersonCreateContact{{Value: "alex@example.com", Type: "work"}},
		Phones: []store.PersonCreateContact{{Value: "+12025550123", Type: "cell"}},
		Org:    "Example Company", Title: "Engineer", Address: "123 Example Street", Note: "Met at a conference",
	})
	require.NoError(err)
	require.Empty(person.ParticipantIDs)
	require.NoError(service.PublishPerson(t.Context(), person.ID))
	fixture.mu.Lock()
	body := string(fixture.body)
	puts := fixture.puts
	fixture.mu.Unlock()
	assert.Equal(1, puts)
	assert.Equal(1, strings.Count(body, "FN:Alex Example\r\n"))
	assert.Contains(body, "UID:"+person.VCardUID)
	assert.Contains(body, "EMAIL;TYPE=work:alex@example.com")
	assert.Contains(body, "TEL;TYPE=cell:+12025550123")
	assert.Contains(body, "ORG:Example Company")
	assert.Contains(body, "TITLE:Engineer")
	assert.Contains(body, "123 Example Street")
	assert.Contains(body, "NOTE:Met at a conference")
}
