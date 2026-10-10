package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strconv"

	"go.kenn.io/msgvault/internal/store"
)

const emlxReceiptVersion = 1

// emlxReceipt is the checksum payload of an emlx-occurrence ledger row.
type emlxReceipt struct {
	Version     int               `json:"version"`
	ID          string            `json:"id"`
	Signature   string            `json:"signature"`
	SourceParts map[string]string `json:"source_parts"`
	Target      string            `json:"target"`
}

// emlxCompletion is the checksum payload of an emlx-target ledger row. Run
// records the sync run that completed the target, so one run completes a
// shared target once even when it forces reconciliation of every occurrence.
// A pending row's Intent names the occurrence whose attachments the
// interrupted ingest was writing.
type emlxCompletion struct {
	Version int         `json:"version"`
	Target  string      `json:"target"`
	Policy  string      `json:"policy"`
	Run     int64       `json:"run,omitzero"`
	Intent  *emlxIntent `json:"intent,omitzero"`
}

// emlxIntent records, before an ingest, which occurrence contributes which
// attachment parts and the digest of the raw being written. If the archived
// raw matches Raw afterwards, the write landed and the parts belong to that
// occurrence, even when the process stopped before its receipt was saved.
type emlxIntent struct {
	Occurrence string            `json:"occurrence"`
	Parts      map[string]string `json:"parts"`
	Raw        string            `json:"raw"`
}

func emlxDigest(s string) string {
	return emlxRawDigest([]byte(s))
}

func emlxRawDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func encodeEmlxReceipt(r emlxReceipt) (string, error) {
	b, err := json.Marshal(r, json.Deterministic(true))
	return string(b), err
}

func decodeEmlxReceipt(s, id string) (emlxReceipt, bool) {
	var r emlxReceipt
	if err := json.Unmarshal([]byte(s), &r, json.RejectUnknownMembers(true)); err != nil {
		return r, false
	}
	for key, hash := range r.SourceParts {
		i, err := strconv.Atoi(key)
		if err != nil || i <= 0 || strconv.Itoa(i) != key || !store.IsEmlxDigest(hash) {
			return r, false
		}
	}
	return r, r.Version == emlxReceiptVersion && r.ID == id && store.IsEmlxOccurrenceID(id) &&
		(r.Signature == "" || store.IsEmlxDigest(r.Signature)) && store.IsEmlxTargetID(r.Target)
}

// emlxTargetCompletion returns the target's completion record when its archived
// raw is live and was completed under policy.
func emlxTargetCompletion(st store.EmlxTargetState, id, policy string) (emlxCompletion, bool) {
	var c emlxCompletion
	if st.MessageID == 0 || !st.HasRaw || st.Deleted || st.Item == nil || st.Item.Status != "imported" {
		return c, false
	}
	if err := json.Unmarshal([]byte(st.Item.Checksum), &c, json.RejectUnknownMembers(true)); err != nil {
		return c, false
	}
	return c, c.Version == emlxReceiptVersion && c.Target == id && c.Policy == policy
}

// emlxPendingIntent returns the intent recorded on a pending target row.
func emlxPendingIntent(st store.EmlxTargetState, id string) *emlxIntent {
	var c emlxCompletion
	if st.Item == nil || st.Item.Status != "pending" ||
		json.Unmarshal([]byte(st.Item.Checksum), &c, json.RejectUnknownMembers(true)) != nil ||
		c.Version != emlxReceiptVersion || c.Target != id || c.Intent == nil ||
		!store.IsEmlxOccurrenceID(c.Intent.Occurrence) || !store.IsEmlxDigest(c.Intent.Raw) {
		return nil
	}
	return c.Intent
}

func targetComplete(st store.EmlxTargetState, id, policy string) bool {
	_, ok := emlxTargetCompletion(st, id, policy)
	return ok
}

func emlxTargetItem(sourceID int64, c emlxCompletion, status string) (store.SourceImportItem, error) {
	c.Version = emlxReceiptVersion
	data, err := json.Marshal(c, json.Deterministic(true))
	return store.SourceImportItem{
		SourceID: sourceID, Provider: "emlx-target", ProviderID: c.Target, Checksum: string(data), Status: status,
	}, err
}

func emlxPolicy(st *store.Store, opts EmlxImportOptions) string {
	dest := opts.AttachmentsDir
	if dest != "" {
		if abs, err := filepath.Abs(dest); err == nil {
			dest = abs
		}
	}
	return emlxDigest(fmt.Sprintf("completion:%d;attachments:%q;fts:%t",
		emlxReceiptVersion, dest, st.FTS5Available()))
}

// emlxSignature binds a filesystem fingerprint to the settings that decide
// what a completed occurrence contributed.
func emlxSignature(fingerprint, label string, maxBytes int64) string {
	return emlxDigest(fmt.Sprintf("%d:%s:%s:%d", emlxReceiptVersion, fingerprint, label, maxBytes))
}
