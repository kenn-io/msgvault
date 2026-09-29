package personenrichment

import (
	"encoding/json/jsontext"
	"errors"

	"go.kenn.io/msgvault/internal/personfacts"
)

func claimsForValues(
	target personfacts.TargetDescriptor,
	values []jsontext.Value,
	score int,
	evidence []personfacts.EvidenceInput,
) ([]personfacts.ProposedClaim, error) {
	claims := make([]personfacts.ProposedClaim, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized, failure, err := personfacts.NormalizeClaimValue(target, value)
		if err != nil || failure != nil || normalized == nil {
			return nil, errors.New("provider returned an unsupported target value")
		}
		// Values for one target share confidence and evidence. Keep the first
		// occurrence of each canonical value.
		if _, duplicate := seen[normalized.Fingerprint]; duplicate {
			continue
		}
		seen[normalized.Fingerprint] = struct{}{}
		claims = append(claims, personfacts.ProposedClaim{
			Target: target, Relation: personfacts.RelationSupport,
			SubmittedValue: append(jsontext.Value(nil), value...), Evidence: evidence,
			Origin:     personfacts.OriginEnrichment,
			Confidence: personfacts.ConfidenceInputs{ReportedScore: score},
		})
	}
	return claims, nil
}
