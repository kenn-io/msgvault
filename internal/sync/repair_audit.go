package sync

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// AuditState is both a field comparison state and the overall result class.
type AuditState string

const (
	AuditMatch        AuditState = "match"
	AuditMismatch     AuditState = "mismatch"
	AuditInconclusive AuditState = "inconclusive"

	AuditFieldRawMIME            = "raw_mime"
	AuditFieldRFC822MessageID    = "rfc822_message_id"
	AuditFieldSubject            = "subject"
	AuditFieldBodyText           = "body_text"
	AuditFieldBodyHTML           = "body_html"
	AuditFieldFrom               = "from"
	AuditFieldTo                 = "to"
	AuditFieldCC                 = "cc"
	AuditFieldBCC                = "bcc"
	AuditFieldAttachmentHashes   = "attachment_hashes"
	AuditFieldAttachmentPartKeys = "attachment_part_keys"

	auditPageSize = 100
)

// RepairAuditResult identifies a corruption candidate or a row whose raw MIME
// could not be audited. Coherent rows are omitted from the stream.
type RepairAuditResult struct {
	InternalID      int64                 `json:"internal_id"`
	SourceID        int64                 `json:"source_id"`
	SourceMessageID string                `json:"source_message_id"`
	Status          AuditState            `json:"status"`
	Fields          map[string]AuditState `json:"fields"`
	Error           string                `json:"error,omitempty"`
}

// AuditGmailMessages strictly parses stored Gmail raw MIME and emits only
// mismatches plus wholly inconclusive rows. sourceID zero audits every Gmail
// source. The store reader and this comparison path perform no writes.
func (s *Syncer) AuditGmailMessages(
	ctx context.Context, sourceID int64, emit func(RepairAuditResult) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sourceID < 0 {
		return errors.New("audit source ID must be positive")
	}
	if sourceID > 0 {
		source, err := s.store.GetSourceByIDContext(ctx, sourceID)
		if err != nil {
			return err
		}
		if source.SourceType != "gmail" {
			return fmt.Errorf("audit source %d is %s, not gmail", sourceID, source.SourceType)
		}
	}
	if emit == nil {
		return errors.New("audit result emitter is required")
	}

	var afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Each page is one read snapshot; records stream one at a time so a
		// page never materializes more than one message's evidence.
		pageSize, err := s.store.StreamGmailAuditEvidencePageContext(
			ctx, sourceID, afterID, auditPageSize,
			func(evidence store.GmailAuditEvidence) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				result, include := auditGmailEvidence(evidence)
				if include {
					if err := ctx.Err(); err != nil {
						return err
					}
					if err := emit(result); err != nil {
						return err
					}
				}
				afterID = evidence.ID
				return ctx.Err()
			})
		if err != nil {
			return err
		}
		if pageSize < auditPageSize {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func auditGmailEvidence(evidence store.GmailAuditEvidence) (RepairAuditResult, bool) {
	result := RepairAuditResult{
		InternalID: evidence.ID, SourceID: evidence.SourceID,
		SourceMessageID: evidence.SourceMessageID,
		Fields:          make(map[string]AuditState),
	}
	if !evidence.RawMIMEPresent {
		result.Status = AuditInconclusive
		result.Fields[AuditFieldRawMIME] = AuditInconclusive
		result.Error = "stored raw MIME is missing"
		return result, true
	}
	if evidence.RawMIMEError != "" {
		result.Status = AuditInconclusive
		result.Fields[AuditFieldRawMIME] = AuditInconclusive
		result.Error = textutil.FirstLine(evidence.RawMIMEError)
		return result, true
	}
	parsed, err := mime.Parse(evidence.RawMIME)
	if err != nil {
		result.Status = AuditInconclusive
		result.Fields[AuditFieldRawMIME] = AuditInconclusive
		result.Error = textutil.FirstLine(err.Error())
		return result, true
	}
	result.Fields[AuditFieldRawMIME] = AuditMatch
	result.Fields[AuditFieldRFC822MessageID] = compareOptionalString(
		evidence.RFC822MessageID.Valid, evidence.RFC822MessageID.String, parsed.MessageID,
	)
	result.Fields[AuditFieldSubject] = compareOptionalString(
		evidence.Subject.Valid, evidence.Subject.String, textutil.EnsureUTF8(parsed.Subject),
	)
	result.Fields[AuditFieldBodyText] = compareOptionalString(
		evidence.BodyText.Valid, evidence.BodyText.String, textutil.EnsureUTF8(parsed.GetBodyText()),
	)
	result.Fields[AuditFieldBodyHTML] = compareOptionalString(
		evidence.BodyHTML.Valid, evidence.BodyHTML.String, textutil.EnsureUTF8(parsed.BodyHTML),
	)

	storedAddresses, complete := storedAuditAddresses(evidence.Recipients)
	parsedAddresses := map[string][]string{
		"from": normalizedAddressMultiset(parsed.From),
		"to":   normalizedAddressMultiset(parsed.To),
		"cc":   normalizedAddressMultiset(parsed.Cc),
		"bcc":  normalizedAddressMultiset(parsed.Bcc),
	}
	for field, recipientType := range map[string]string{
		AuditFieldFrom: "from", AuditFieldTo: "to", AuditFieldCC: "cc", AuditFieldBCC: "bcc",
	} {
		if !complete[recipientType] {
			result.Fields[field] = AuditInconclusive
		} else {
			result.Fields[field] = compareStrings(storedAddresses[recipientType], parsedAddresses[recipientType])
		}
	}

	storedHashes := make([]string, 0, len(evidence.Attachments))
	storedKeys := make([]string, 0, len(evidence.Attachments))
	storedPairs := make([][2]string, 0, len(evidence.Attachments))
	hashesComplete, keysComplete := true, true
	for _, attachment := range evidence.Attachments {
		if !attachment.ContentHash.Valid || attachment.ContentHash.String == "" {
			hashesComplete = false
		} else {
			storedHashes = append(storedHashes, strings.ToLower(attachment.ContentHash.String))
		}
		if !attachment.SourcePartKey.Valid || attachment.SourcePartKey.String == "" {
			keysComplete = false
		} else {
			storedKeys = append(storedKeys, attachment.SourcePartKey.String)
		}
		if attachment.ContentHash.Valid && attachment.ContentHash.String != "" &&
			attachment.SourcePartKey.Valid && attachment.SourcePartKey.String != "" {
			storedPairs = append(storedPairs, [2]string{
				attachment.SourcePartKey.String,
				strings.ToLower(attachment.ContentHash.String),
			})
		}
	}
	parsedHashes := make([]string, 0, len(parsed.Attachments))
	parsedKeys := make([]string, 0, len(parsed.Attachments))
	parsedPairs := make([][2]string, 0, len(parsed.Attachments))
	for _, attachment := range parsed.Attachments {
		if attachment.ContentHash == "" {
			hashesComplete = false
		} else {
			parsedHashes = append(parsedHashes, strings.ToLower(attachment.ContentHash))
		}
		if attachment.PartKey == "" {
			keysComplete = false
		} else {
			parsedKeys = append(parsedKeys, attachment.PartKey)
		}
		if attachment.ContentHash != "" && attachment.PartKey != "" {
			parsedPairs = append(parsedPairs, [2]string{
				attachment.PartKey,
				strings.ToLower(attachment.ContentHash),
			})
		}
	}
	if !hashesComplete {
		result.Fields[AuditFieldAttachmentHashes] = AuditInconclusive
	} else if keysComplete {
		// Equal hash and part-key multisets do not prove coherence: each hash
		// must belong to the part key it was stored under, so compare the
		// normalized (part key, content hash) pairs as a multiset.
		result.Fields[AuditFieldAttachmentHashes] = compareAttachmentPairs(storedPairs, parsedPairs)
	} else {
		result.Fields[AuditFieldAttachmentHashes] = compareUnkeyedAttachmentHashes(storedHashes, parsedHashes)
	}
	if keysComplete {
		result.Fields[AuditFieldAttachmentPartKeys] = compareStrings(storedKeys, parsedKeys)
	} else {
		result.Fields[AuditFieldAttachmentPartKeys] = AuditInconclusive
	}

	matched := false
	for _, state := range result.Fields {
		if state == AuditMismatch {
			result.Status = AuditMismatch
			return result, true
		}
		matched = matched || state == AuditMatch
	}
	if !matched {
		result.Status = AuditInconclusive
		return result, true
	}
	return result, false
}

func compareOptionalString(storedValid bool, stored, parsed string) AuditState {
	if !storedValid {
		return AuditInconclusive
	}
	if stored == parsed {
		return AuditMatch
	}
	return AuditMismatch
}

func storedAuditAddresses(recipients []store.GmailAuditRecipient) (map[string][]string, map[string]bool) {
	addresses := map[string][]string{"from": {}, "to": {}, "cc": {}, "bcc": {}}
	complete := map[string]bool{"from": true, "to": true, "cc": true, "bcc": true}
	for _, recipient := range recipients {
		if _, relevant := complete[recipient.Type]; !relevant {
			continue
		}
		if !recipient.EmailAddress.Valid {
			complete[recipient.Type] = false
			continue
		}
		addresses[recipient.Type] = append(addresses[recipient.Type], normalizeAuditAddress(recipient.EmailAddress.String))
	}
	for recipientType := range addresses {
		sort.Strings(addresses[recipientType])
	}
	return addresses, complete
}

func normalizedAddressMultiset(addresses []mime.Address) []string {
	result := make([]string, 0, len(addresses))
	seen := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		normalized := normalizeAuditAddress(address.Email)
		if normalized == "" {
			continue
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	sort.Strings(result)
	return result
}

func normalizeAuditAddress(address string) string {
	return strings.ToLower(strings.TrimSpace(address))
}

// compareUnkeyedAttachmentHashes audits rows written before occurrence keys
// existed. Such rows are unique per (message, content hash), so a part that
// repeats in the MIME was stored once, and older ingests recorded small
// nameless text parts as attachments that the current parser folds into the
// body. A parsed hash the row never stored means the raw MIME carries a part
// this message never had, which is the cross-assignment signature. A stored
// hash the parse no longer yields is classification drift, not evidence.
func compareUnkeyedAttachmentHashes(stored, parsed []string) AuditState {
	storedSet := make(map[string]struct{}, len(stored))
	for _, hash := range stored {
		storedSet[hash] = struct{}{}
	}
	parsedSet := make(map[string]struct{}, len(parsed))
	for _, hash := range parsed {
		if _, known := storedSet[hash]; !known {
			return AuditMismatch
		}
		parsedSet[hash] = struct{}{}
	}
	if len(parsedSet) == len(storedSet) {
		return AuditMatch
	}
	return AuditInconclusive
}

func compareStrings(left, right []string) AuditState {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	if len(left) != len(right) {
		return AuditMismatch
	}
	for i := range left {
		if left[i] != right[i] {
			return AuditMismatch
		}
	}
	return AuditMatch
}

func compareAttachmentPairs(stored, parsed [][2]string) AuditState {
	stored = append([][2]string(nil), stored...)
	parsed = append([][2]string(nil), parsed...)
	sortAttachmentPairs(stored)
	sortAttachmentPairs(parsed)
	if len(stored) != len(parsed) {
		return AuditMismatch
	}
	for i := range stored {
		if stored[i] != parsed[i] {
			return AuditMismatch
		}
	}
	return AuditMatch
}

func sortAttachmentPairs(pairs [][2]string) {
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
}
