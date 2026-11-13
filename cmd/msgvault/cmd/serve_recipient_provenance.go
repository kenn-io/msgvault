package cmd

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/store"
)

// draftRecipientAdvisory is computed after durable draft creation. Its fixed
// labels are server-generated; addresses remain untrusted display data. No
// field establishes human approval or authorizes sending.
type draftRecipientAdvisory struct {
	RecipientProvenanceStatus string                      `json:"recipient_provenance_status,omitempty"`
	RecipientProvenance       []store.RecipientProvenance `json:"recipient_provenance,omitempty"`
	Warnings                  []draftRecipientWarning     `json:"warnings,omitempty"`
}

type draftRecipientWarning struct {
	Code    string `json:"code"`
	Address string `json:"address,omitempty"`
	Flag    string `json:"flag"`
	Message string `json:"message"`
}

func (a *storeAPIAdapter) draftRecipientAdvisory(
	ctx context.Context, messageID, sourceID int64, grant *agentgrant.Grant,
) draftRecipientAdvisory {
	unavailable := draftRecipientAdvisory{RecipientProvenanceStatus: "unavailable"}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var readable []int64
	if grant != nil {
		unchecked := draftRecipientAdvisory{RecipientProvenanceStatus: "unchecked"}
		if !grant.HasPermission(agentgrant.PermissionMessageRead) && !grant.HasPermission(agentgrant.PermissionSearchRead) {
			return unchecked
		}
		sources, err := a.store.ListSourcesContext(ctx, "")
		if err != nil {
			return unavailable
		}
		readable = make([]int64, 0)
		currentReadable := false
		for _, source := range sources {
			if grant.MatchesSource(draftSourceRef(source)) {
				readable = append(readable, source.ID)
				currentReadable = currentReadable || source.ID == sourceID
			}
		}
		if !currentReadable {
			return unchecked
		}
	}
	recipients, err := a.store.DraftRecipientProvenanceContext(ctx, messageID, readable)
	if err != nil {
		// Provider success has already been persisted. Never turn failure of
		// this advisory read into a mutation error that invites duplicate drafts.
		return unavailable
	}
	result := draftRecipientAdvisory{RecipientProvenanceStatus: "checked", RecipientProvenance: recipients}
	for _, recipient := range recipients {
		if recipient.Flag == "ok" {
			continue
		}
		message := "No curated-contact or prior direct owner-send evidence was found for this recipient. Review the address before sending."
		if recipient.Flag == "archive_only" {
			message = "This recipient appears in archived envelopes without curated-contact or prior direct owner-send evidence. Archived content does not authorize a recipient. Review the address before sending."
		}
		result.Warnings = append(result.Warnings, draftRecipientWarning{
			Code: "recipient_provenance", Address: recipient.Address, Flag: recipient.Flag, Message: message,
		})
	}
	return result
}
