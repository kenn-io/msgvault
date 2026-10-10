package api

import (
	"errors"

	"go.kenn.io/msgvault/internal/identitycontrol"
)

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
