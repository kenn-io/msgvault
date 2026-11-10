package cmd

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/draftlint"
	"go.kenn.io/msgvault/internal/store"
)

// draftRecipientAdvisory is computed after durable draft creation. Its fixed
// labels are server-generated; addresses remain untrusted display data. No
// field establishes human approval or authorizes sending.
type draftRecipientAdvisory struct {
	RecipientProvenanceStatus string                      `json:"recipient_provenance_status,omitempty"`
	RecipientProvenance       []store.RecipientProvenance `json:"recipient_provenance,omitempty"`
	Warnings                  []draftlint.Warning         `json:"warnings,omitempty"`
	LintSkipped               []string                    `json:"lint_skipped,omitempty"`
}

func (a *storeAPIAdapter) draftRecipientAdvisory(
	ctx context.Context, messageID, sourceID int64, grant *agentgrant.Grant,
) draftRecipientAdvisory {
	unavailable := draftRecipientAdvisory{RecipientProvenanceStatus: "unavailable"}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	readable, status := a.draftReadableSources(ctx, sourceID, grant)
	if status != "checked" {
		return draftRecipientAdvisory{RecipientProvenanceStatus: status}
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
		result.Warnings = append(result.Warnings, draftlint.Warning{
			Code: draftlint.CodeRecipientProvenance, Severity: "warn", Address: recipient.Address, Flag: recipient.Flag, Message: message,
		})
	}
	return result
}

// draftReadableSources keeps lint and post-write provenance on the same grant
// policy. A nil scope means owner access; unchecked never permits a lookup.
func (a *storeAPIAdapter) draftReadableSources(ctx context.Context, sourceID int64, grant *agentgrant.Grant) ([]int64, string) {
	if grant == nil {
		return nil, "checked"
	}
	if !grant.HasPermission(agentgrant.PermissionMessageRead) && !grant.HasPermission(agentgrant.PermissionSearchRead) {
		return nil, "unchecked"
	}
	sources, err := a.store.ListSourcesContext(ctx, "")
	if err != nil {
		return nil, "unavailable"
	}
	readable := make([]int64, 0)
	current := false
	for _, source := range sources {
		if grant.MatchesSource(draftSourceRef(source)) {
			readable = append(readable, source.ID)
			current = current || source.ID == sourceID
		}
	}
	if !current {
		return nil, "unchecked"
	}
	return readable, "checked"
}
