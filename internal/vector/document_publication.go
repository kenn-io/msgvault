package vector

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// ValidatedScopePublication is one scope that passed ValidateScopePublications.
type ValidatedScopePublication struct {
	Publication DocumentScopePublication
	DocByMember map[int64]string
	DesiredKeys []string
}

// ValidateScopePublications checks a publication batch before any backend work.
func ValidateScopePublications(scopes []DocumentScopePublication) ([]ValidatedScopePublication, error) {
	validated := make([]ValidatedScopePublication, len(scopes))
	seenScopes := make(map[string]struct{}, len(scopes))
	for i, publication := range scopes {
		if publication.ScopeKey == "" {
			return nil, errors.New("publish scope: empty scope key")
		}
		if _, exists := seenScopes[publication.ScopeKey]; exists {
			return nil, fmt.Errorf("publish scopes: duplicate scope key %q", publication.ScopeKey)
		}
		seenScopes[publication.ScopeKey] = struct{}{}
		docByMember, desiredKeys, err := validateDocumentPublication(
			publication.SourceSequence, publication.Documents, publication.Chunks, publication.FenceOnly)
		if err != nil {
			return nil, err
		}
		validated[i] = ValidatedScopePublication{
			Publication: publication, DocByMember: docByMember, DesiredKeys: desiredKeys,
		}
	}
	return validated, nil
}

func validateDocumentPublication(
	sourceSequence int64, docs []DocumentPublication, chunks []Chunk, fenceOnly bool,
) (map[int64]string, []string, error) {
	if fenceOnly && len(chunks) != 0 {
		return nil, nil, errors.New("publish scope: fence-only publication cannot contain chunks")
	}
	docByMember := make(map[int64]string)
	preserved := make(map[int64]struct{})
	seenKeys := make(map[string]struct{}, len(docs))
	keys := make([]string, 0, len(docs))
	for _, doc := range docs {
		if doc.SourceSequence != sourceSequence {
			return nil, nil, fmt.Errorf("publish scope: document %q source sequence %d does not match scope sequence %d", doc.Key, doc.SourceSequence, sourceSequence)
		}
		if doc.Key == "" || doc.Kind == "" || doc.Revision == "" {
			return nil, nil, errors.New("publish scope: document key, kind, and revision are required")
		}
		if _, exists := seenKeys[doc.Key]; exists {
			return nil, nil, fmt.Errorf("publish scope: duplicate document key %q", doc.Key)
		}
		seenKeys[doc.Key] = struct{}{}
		keys = append(keys, doc.Key)
		for _, messageID := range doc.Members {
			if owner, exists := docByMember[messageID]; exists {
				return nil, nil, fmt.Errorf("publish scope: message %d belongs to both %q and %q", messageID, owner, doc.Key)
			}
			docByMember[messageID] = doc.Key
			if doc.PreserveVectors {
				preserved[messageID] = struct{}{}
			}
		}
	}
	chunked := make(map[int64]struct{}, len(chunks))
	for _, chunk := range chunks {
		if _, exists := docByMember[chunk.MessageID]; !exists {
			return nil, nil, fmt.Errorf("publish scope: chunk message %d has no desired document owner", chunk.MessageID)
		}
		if _, exists := preserved[chunk.MessageID]; exists {
			return nil, nil, fmt.Errorf("publish scope: preserved document member %d also has a replacement chunk", chunk.MessageID)
		}
		chunked[chunk.MessageID] = struct{}{}
	}
	for messageID := range preserved {
		chunked[messageID] = struct{}{}
	}
	if !fenceOnly {
		for messageID := range docByMember {
			if _, exists := chunked[messageID]; !exists {
				return nil, nil, fmt.Errorf("publish scope: document member %d has no chunk", messageID)
			}
		}
	}
	slices.Sort(keys)
	return docByMember, keys, nil
}

// PreservedDocumentMembers returns the members whose vectors a publication
// keeps, failing when a preserved document no longer matches the ledger.
func PreservedDocumentMembers(current []DocumentRecord, desired []DocumentPublication) (map[int64]struct{}, error) {
	currentByKey := make(map[string]DocumentRecord, len(current))
	for _, record := range current {
		currentByKey[record.Key] = record
	}
	preserved := make(map[int64]struct{})
	for _, doc := range desired {
		if !doc.PreserveVectors {
			continue
		}
		record, ok := currentByKey[doc.Key]
		if !ok || record.Kind != doc.Kind || record.PublishedRevision != doc.Revision ||
			!slices.Equal(record.Members, doc.Members) {
			return nil, fmt.Errorf("%w: preserved document %q changed", ErrDocumentFenceChanged, doc.Key)
		}
		for _, messageID := range doc.Members {
			preserved[messageID] = struct{}{}
		}
	}
	return preserved, nil
}

// SameFenceDocuments reports whether a fence-only publication names exactly
// the current documents of its scope.
func SameFenceDocuments(current []DocumentRecord, desired []DocumentPublication) bool {
	if len(current) != len(desired) {
		return false
	}
	desiredByKey := make(map[string]DocumentPublication, len(desired))
	for _, doc := range desired {
		desiredByKey[doc.Key] = doc
	}
	for _, record := range current {
		doc, ok := desiredByKey[record.Key]
		if !ok || record.Kind != doc.Kind || record.PublishedRevision != doc.Revision ||
			!slices.Equal(record.Members, doc.Members) {
			return false
		}
	}
	return true
}

// ScanScopeDocuments groups (key, kind, revision, member) rows ordered by
// document key and member ordinal into records, then closes rows.
func ScanScopeDocuments(rows *sql.Rows, scopeKey string) ([]DocumentRecord, error) {
	defer func() { _ = rows.Close() }()
	records := make([]DocumentRecord, 0)
	for rows.Next() {
		var key, kind, revision string
		var member sql.NullInt64
		if err := rows.Scan(&key, &kind, &revision, &member); err != nil {
			return nil, fmt.Errorf("scan scope %q document fence: %w", scopeKey, err)
		}
		if len(records) == 0 || records[len(records)-1].Key != key {
			records = append(records, DocumentRecord{Key: key, Kind: kind,
				PublishedRevision: revision, Members: []int64{}})
		}
		if member.Valid {
			records[len(records)-1].Members = append(records[len(records)-1].Members, member.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read scope %q document fence rows: %w", scopeKey, err)
	}
	return records, nil
}
