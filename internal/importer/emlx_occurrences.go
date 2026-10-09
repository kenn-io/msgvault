package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

const emlxReceiptVersion = 1

type emlxReceipt struct {
	Version     int               `json:"version"`
	ID          string            `json:"id"`
	Signature   string            `json:"signature"`
	SourceParts map[string]string `json:"source_parts"`
	Target      string            `json:"target"`
	RFCID       string            `json:"rfc_id"`
	Reply       string            `json:"reply"`
}
type emlxCompletion struct {
	Version int    `json:"version"`
	Target  string `json:"target"`
	Policy  string `json:"policy"`
}

func emlxDigest(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func validEmlxDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'f') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
func validEmlxRelative(s string) bool {
	return filepath.IsLocal(s) && filepath.ToSlash(s) == s && path.Clean(s) == s && !strings.ContainsRune(s, '\x00')
}
func validEmlxTarget(s string) bool {
	return strings.HasPrefix(s, "emlx-") && validEmlxDigest(strings.TrimPrefix(s, "emlx-"))
}
func validEmlxOccurrence(s string) bool {
	return len(s) > 65 && s[64] == '/' && validEmlxDigest(s[:64]) && validEmlxRelative(s[65:])
}
func encodeEmlxReceipt(r emlxReceipt) (string, error) {
	b, err := json.Marshal(r, json.Deterministic(true))
	return string(b), err
}
func decodeEmlxReceipt(s, id string) (emlxReceipt, bool) {
	var r emlxReceipt
	err := json.Unmarshal([]byte(s), &r, json.RejectUnknownMembers(true))
	for key, hash := range r.SourceParts {
		i, parseErr := strconv.Atoi(key)
		if parseErr != nil || i <= 0 || strconv.Itoa(i) != key || !validEmlxDigest(hash) {
			return r, false
		}
	}
	return r, err == nil && r.Version == emlxReceiptVersion && r.ID == id && validEmlxOccurrence(id) &&
		(r.Signature == "" || validEmlxDigest(r.Signature)) && validEmlxTarget(r.Target)
}
func targetComplete(st store.EmlxTargetState, id, policy string) bool {
	if st.MessageID == 0 || !st.HasRaw || st.Deleted || st.Item == nil || st.Item.Status != "imported" {
		return false
	}
	var c emlxCompletion
	return json.Unmarshal([]byte(st.Item.Checksum), &c, json.RejectUnknownMembers(true)) == nil && c.Version == emlxReceiptVersion && c.Target == id && c.Policy == policy && validEmlxDigest(c.Policy)
}
func putEmlxTarget(ctx context.Context, st *store.Store, sourceID int64, id, policy, status string) error {
	data, err := json.Marshal(emlxCompletion{Version: emlxReceiptVersion, Target: id, Policy: policy}, json.Deterministic(true))
	if err != nil {
		return err
	}
	return st.PutEmlxLedgerItemContext(ctx, store.SourceImportItem{SourceID: sourceID, Provider: "emlx-target", ProviderID: id, Checksum: string(data), Status: status})
}
func emlxPolicy(st *store.Store, opts EmlxImportOptions) string {
	dest := opts.AttachmentsDir
	if dest != "" {
		if abs, err := filepath.Abs(dest); err == nil {
			dest = abs
		}
	}
	return emlxDigest(fmt.Sprintf("completion:%d;attachments:%q;fts:%t", emlxReceiptVersion, dest, st.FTS5Available()))
}
