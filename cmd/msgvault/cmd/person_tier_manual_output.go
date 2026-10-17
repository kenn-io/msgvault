package cmd

import (
	"encoding/json/v2"
	"io"

	"go.kenn.io/msgvault/internal/textutil"
)

// The provenance key was added with opt-out reconciliation in API 3.24. Older
// manual routes remain usable; their removal responses promise no new side effect.
func personTierHasProvenance(body []byte) bool {
	var response struct {
		Provenance *string `json:"provenance"`
	}
	return json.Unmarshal(body, &response) == nil && response.Provenance != nil
}

func writePersonTierManualGrant(w io.Writer, provenance, actor string) error {
	if provenance == "" {
		provenance = "manual"
	}
	return personTierPrintf(w, "provenance: %s; actor: %s\n", textutil.SanitizeTerminal(provenance), personTierActor(actor))
}

func writePersonTierRemoval(w io.Writer, body []byte, personID int64, lanes ...string) error {
	if err := personTierPrintf(w, "provenance: none\n"); err != nil {
		return err
	}
	if !personTierHasProvenance(body) {
		return nil
	}
	label := "recorded opt-out"
	if len(lanes) > 1 {
		label += "s"
	}
	if err := personTierPrintf(w, "%s; reverse with:\n", label); err != nil {
		return err
	}
	for _, lane := range lanes {
		if err := personTierPrintf(w, "  msgvault person tiers opt-in %d --lane %s\n", personID, lane); err != nil {
			return err
		}
	}
	return nil
}
