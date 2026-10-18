package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContactGrantSelectionOpenAPIContract(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	doc := OpenAPIDocument()
	request := doc.Components.Schemas.Map()["AgentTokenIssueRequest"]
	requirements.NotNil(request)
	for _, name := range []string{"source_ids", "person_ids", "address_book_ids"} {
		field := request.Properties[name]
		requirements.NotNil(field, name)
		assertions.NotContains(request.Required, name)
		requirements.NotNil(field.Items, name)
		assertions.Equal(new(float64(1)), field.Items.Minimum, name)
		assertions.Equal(new(float64(9007199254740991)), field.Items.Maximum, name)
		assertions.Equal(new(100), field.MaxItems, name)
		assertions.True(field.UniqueItems, name)
	}
	for _, name := range []string{"AgentTokenIssueResponse", "AgentTokenView"} {
		view := doc.Components.Schemas.Map()[name]
		requirements.NotNil(view)
		for _, field := range []string{"sources", "persons", "address_books"} {
			assertions.Contains(view.Properties, field, name)
		}
		assertions.Contains(view.Properties, "expires_at", name)
	}
	// Keep both contracts of main's merged durable UID lookup.
	assertions.NotNil(doc.Paths["/api/v1/people/by-uid"].Get)
	person := doc.Components.Schemas.Map()["Person"]
	requirements.NotNil(person)
	assertions.Contains(person.Properties, "vcard_uid")
	assertions.Contains(person.Properties, "carddav_bindings")
}
