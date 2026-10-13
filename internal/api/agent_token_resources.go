package api

import (
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/identitycontrol"
)

func hardenContactGrantSchemas(doc *huma.OpenAPI) {
	request := doc.Components.Schemas.Map()["AgentTokenIssueRequest"]
	if request == nil {
		return
	}
	request.Description = "Select 1–100 explicit resources in total across sources, people and address books."
	for _, name := range []string{"source_ids", "person_ids", "address_book_ids"} {
		field := request.Properties[name]
		if field == nil || field.Items == nil {
			continue
		}
		field.MaxItems = new(100)
		field.UniqueItems = true
		field.Items.Minimum = new(float64(1))
		field.Items.Maximum = new(float64(9007199254740991))
	}
}

func validateAgentTokenResourceSelection(request agentTokenIssueRequest) error {
	count := len(request.SourceIDs) + len(request.PersonIDs) + len(request.AddressBookIDs)
	if count < 1 || count > 100 {
		return errors.New("select between 1 and 100 explicit sources, people or address books")
	}
	for _, ids := range [][]int64{request.SourceIDs, request.PersonIDs, request.AddressBookIDs} {
		seen := make(map[int64]bool, len(ids))
		for _, id := range ids {
			if !identitycontrol.ValidID(id) || seen[id] {
				return errors.New("resource IDs must be distinct exact positive JSON integers")
			}
			seen[id] = true
		}
	}
	return nil
}

func grantPersonViews(refs []agentgrant.PersonRef) []agentTokenPersonView {
	views := make([]agentTokenPersonView, len(refs))
	for i, ref := range refs {
		views[i] = agentTokenPersonView{ID: ref.ID, UID: ref.UID}
	}
	return views
}

func grantAddressBookViews(refs []agentgrant.AddressBookRef) []agentTokenAddressBookView {
	views := make([]agentTokenAddressBookView, len(refs))
	for i, ref := range refs {
		views[i] = agentTokenAddressBookView{AccountID: ref.AccountID, BookID: ref.BookID, CanonicalURL: ref.CanonicalURL, OwnershipFingerprint: ref.OwnershipFingerprint}
	}
	return views
}
