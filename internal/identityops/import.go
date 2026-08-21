package identityops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/store"
)

// ImportEntry is one provider-neutral identity supplied by a user-owned file.
// State is report-only metadata and never requests removal.
type ImportEntry struct {
	Identifier string `json:"identifier"`
	State      string `json:"state,omitempty"`
}

type ImportRequest struct {
	SourceSelector

	Entries []ImportEntry `json:"entries" nullable:"false"`
	Signal  string        `json:"signal,omitempty"`
	Apply   bool          `json:"apply,omitempty"`
}

func (r *ImportRequest) UnmarshalJSON(data []byte) error {
	type importRequest ImportRequest
	var decoded importRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	set, err := jsonFieldPresent(data, "source_id")
	if err != nil {
		return err
	}
	decoded.SourceIDSet = set
	*r = ImportRequest(decoded)
	return nil
}

type ImportResult struct {
	Account    string                              `json:"account"`
	SourceID   int64                               `json:"source_id"`
	Signal     string                              `json:"signal"`
	Candidates []Candidate                         `json:"candidates" nullable:"false"`
	Applied    []store.IdentityConfirmationOutcome `json:"applied" nullable:"false"`
}

var errTrailingImportJSON = errors.New("identity import must contain a single JSON document")

// ParseImport reads text or strict JSON identity input, validates every row,
// merges case-folded duplicates, and returns stable normalized ordering.
func ParseImport(r io.Reader, format string) ([]ImportEntry, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read identity import: %w", err)
	}
	resolved, err := resolveImportFormat(format, data)
	if err != nil {
		return nil, err
	}

	var entries []ImportEntry
	switch resolved {
	case "text":
		entries = parseTextImport(data)
	case "json":
		entries, err = parseJSONImport(data)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported identity import format %q", format)
	}
	return normalizeImportEntries(entries)
}

// Import validates one complete parsed request, previews every entry, and
// optionally confirms it through the same bounded source-scoped write path as
// discovery. State is copied into the report but never affects confirmation.
func Import(
	ctx context.Context,
	st DiscoveryStore,
	req ImportRequest,
) (ImportResult, error) {
	if err := ctx.Err(); err != nil {
		return ImportResult{}, err
	}
	signal := strings.TrimSpace(req.Signal)
	if signal == "" {
		signal = "manual"
	}
	if strings.Contains(signal, ",") {
		return ImportResult{}, opserr.Invalid(fmt.Errorf("signal names cannot contain commas: %q", signal))
	}
	entries, err := normalizeImportEntries(req.Entries)
	if err != nil {
		return ImportResult{}, opserr.Invalid(err)
	}

	src, err := ResolveSource(st, req.SourceSelector)
	if err != nil {
		return ImportResult{}, err
	}
	existing, err := st.ListAccountIdentities(src.ID)
	if err != nil {
		return ImportResult{}, opserr.Internal(fmt.Errorf("list account identities: %w", err))
	}
	evidence := make([]ExternalEvidence, len(entries))
	for i, entry := range entries {
		evidence[i] = ExternalEvidence{
			Identifier: entry.Identifier,
			Signal:     signal,
			State:      entry.State,
			Strong:     true,
		}
	}
	result := ImportResult{
		Account:    src.Identifier,
		SourceID:   src.ID,
		Signal:     signal,
		Candidates: []Candidate{},
		Applied:    []store.IdentityConfirmationOutcome{},
	}
	discovery := DiscoverResult{SourceID: src.ID, Candidates: result.Candidates}
	seedExistingExternalCandidates(&discovery, evidence, existing)
	MergeExternalEvidence(&discovery, evidence)
	result.Candidates = discovery.Candidates
	if !req.Apply {
		return result, nil
	}

	confirmations := strongConfirmations(result.Candidates, existing)
	if len(confirmations) == 0 {
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.Applied, err = st.AddAccountIdentitiesBatchContext(ctx, src.ID, confirmations)
	return result, err
}

func resolveImportFormat(hint string, data []byte) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(hint))
	switch normalized {
	case "", "auto":
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '{') {
			return "json", nil
		}
		return "text", nil
	case "text", "txt", ".txt", ".text":
		return "text", nil
	case "json", ".json":
		return "json", nil
	}
	switch strings.ToLower(filepath.Ext(normalized)) {
	case ".txt", ".text":
		return "text", nil
	case ".json":
		return "json", nil
	default:
		return "", fmt.Errorf("unsupported identity import format %q", hint)
	}
}

func parseTextImport(data []byte) []ImportEntry {
	lines := strings.Split(string(data), "\n")
	entries := make([]ImportEntry, 0, len(lines))
	for _, line := range lines {
		identifier := strings.TrimSpace(line)
		if identifier == "" || strings.HasPrefix(identifier, "#") {
			continue
		}
		entries = append(entries, ImportEntry{Identifier: identifier})
	}
	return entries
}

func parseJSONImport(data []byte) ([]ImportEntry, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("identity import contains no identities")
	}
	switch trimmed[0] {
	case '[':
		var identifiers []string
		stringErr := decodeStrictImportJSON(trimmed, &identifiers)
		if stringErr == nil {
			entries := make([]ImportEntry, len(identifiers))
			for i, identifier := range identifiers {
				entries[i] = ImportEntry{Identifier: identifier}
			}
			return entries, nil
		}
		if errors.Is(stringErr, errTrailingImportJSON) {
			return nil, stringErr
		}
		var entries []ImportEntry
		entryErr := decodeStrictImportJSON(trimmed, &entries)
		if entryErr == nil {
			return entries, nil
		}
		if errors.Is(entryErr, errTrailingImportJSON) {
			return nil, entryErr
		}
		if strings.Contains(entryErr.Error(), "unknown field") {
			return nil, fmt.Errorf("decode identity import JSON: %w", entryErr)
		}
		return nil, fmt.Errorf("identity import JSON must be an array of strings or identity entries: %w", entryErr)
	case '{':
		var envelope struct {
			Identities []ImportEntry `json:"identities"`
		}
		if err := decodeStrictImportJSON(trimmed, &envelope); err != nil {
			return nil, fmt.Errorf("decode identity import JSON: %w", err)
		}
		return envelope.Identities, nil
	default:
		return nil, errors.New("identity import JSON must be an array or identities object")
	}
}

func decodeStrictImportJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errTrailingImportJSON
		}
		return fmt.Errorf("%w: %w", errTrailingImportJSON, err)
	}
	return nil
}

func normalizeImportEntries(entries []ImportEntry) ([]ImportEntry, error) {
	byNormalized := make(map[string]ImportEntry, len(entries))
	for _, entry := range entries {
		identifier, reason := validateDiscoveredIdentifier(entry.Identifier)
		if reason != "" {
			return nil, fmt.Errorf("invalid imported identity %q: %s", strings.TrimSpace(entry.Identifier), reason)
		}
		normalized := store.NormalizeIdentifierForCompare(identifier)
		entry = ImportEntry{Identifier: identifier, State: strings.TrimSpace(entry.State)}
		if existing, ok := byNormalized[normalized]; ok {
			if existing.State != entry.State {
				return nil, fmt.Errorf(
					"duplicate imported identity %q has conflicting states %q and %q",
					existing.Identifier,
					existing.State,
					entry.State,
				)
			}
			continue
		}
		byNormalized[normalized] = entry
	}
	if len(byNormalized) == 0 {
		return nil, errors.New("identity import contains no identities")
	}

	normalized := make([]string, 0, len(byNormalized))
	for identifier := range byNormalized {
		normalized = append(normalized, identifier)
	}
	sort.Strings(normalized)
	out := make([]ImportEntry, len(normalized))
	for i, identifier := range normalized {
		out[i] = byNormalized[identifier]
	}
	return out, nil
}
