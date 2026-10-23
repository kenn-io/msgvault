package novelty

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"slices"
)

func digestJSON(v any) (string, error) {
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (p Params) Fingerprint() (string, error) {
	return digestJSON(struct {
		PolicyVersion string
		Params        Params
	}{PolicyVersion, p})
}

func AnchorSetSHA256(messageIDs []int64) string {
	if len(messageIDs) == 0 {
		return ""
	}
	ids := slices.Clone(messageIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	digest, _ := digestJSON(ids)
	return digest
}

type DigestInput struct {
	ParamsFingerprint                  string
	Mode                               Mode
	Fallback                           FallbackReason
	VectorFingerprint, AnchorSetSHA256 string
	CandidateKeys                      []string
	Picks                              []Pick
}

func SelectionSHA256(d DigestInput) (string, error) {
	keys := slices.Clone(d.CandidateKeys)
	slices.Sort(keys)
	picks := make([][2]string, 0, len(d.Picks))
	for _, p := range d.Picks {
		picks = append(picks, [2]string{p.Key, string(p.Lane)})
	}
	return digestJSON(struct {
		PolicyVersion     string         `json:"policy_version"`
		ParamsFingerprint string         `json:"params_fingerprint"`
		Mode              Mode           `json:"mode"`
		Fallback          FallbackReason `json:"fallback_reason"`
		VectorFingerprint string         `json:"vector_fingerprint"`
		AnchorSetSHA256   string         `json:"anchor_set_sha256"`
		CandidateKeys     []string       `json:"candidate_keys"`
		Picks             [][2]string    `json:"picks"`
	}{PolicyVersion, d.ParamsFingerprint, d.Mode, d.Fallback, d.VectorFingerprint, d.AnchorSetSHA256, keys, picks})
}
