package config

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
	"go.kenn.io/msgvault/internal/personenrichment"
)

const personEnrichmentProviderEditKey = "people.enrichment.providers"

type personEnrichmentProviderEdit struct {
	name     string
	provider personenrichment.ProviderConfig
}

// EditPersonEnrichmentProvider conditionally updates one provider table by its
// stable name. Unlike a collection round trip, it leaves comments, extension
// keys, host-owned fields, and unrelated provider tables untouched.
func EditPersonEnrichmentProvider(
	path, ifMatch, name string,
	provider personenrichment.ProviderConfig,
) (ConfigFile, error) {
	if name == "" || provider.Name != name {
		return ConfigFile{}, fmt.Errorf("%w: person-enrichment provider name does not match target", ErrAmbiguousConfigTarget)
	}
	return EditConfigFilePrivate(path, ifMatch, []Edit{{
		Key: personEnrichmentProviderEditKey,
		Value: personEnrichmentProviderEdit{
			name:     name,
			provider: provider,
		},
	}})
}

type providerLineRange struct {
	start int
	end   int
	name  string
}

func replacePersonEnrichmentProviderLines(
	lines []tomlLine,
	target personEnrichmentProviderEdit,
) ([]tomlLine, error) {
	ranges, err := personEnrichmentProviderRanges(lines)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(ranges))
	targetIndex := -1
	for index, providerRange := range ranges {
		if _, duplicate := seen[providerRange.name]; duplicate {
			return nil, fmt.Errorf("%w: duplicate person-enrichment provider name %q",
				ErrAmbiguousConfigTarget, providerRange.name)
		}
		seen[providerRange.name] = struct{}{}
		if providerRange.name == target.name {
			targetIndex = index
		}
	}
	if targetIndex < 0 {
		return appendPersonEnrichmentProvider(lines, target.provider)
	}

	providerRange := ranges[targetIndex]
	block := append([]tomlLine(nil), lines[providerRange.start:providerRange.end]...)
	block, err = editPersonEnrichmentProviderBlock(block, target.provider)
	if err != nil {
		return nil, err
	}
	result := make([]tomlLine, 0, len(lines)-providerRange.end+providerRange.start+len(block))
	result = append(result, lines[:providerRange.start]...)
	result = append(result, block...)
	result = append(result, lines[providerRange.end:]...)
	return result, nil
}

func personEnrichmentProviderRanges(lines []tomlLine) ([]providerLineRange, error) {
	structural := tomlStructuralLines(lines)
	var starts []int
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		path, array, ok := parseTOMLTable(line.body)
		if ok && array && equalPath(path, []string{"people", "enrichment", "providers"}) {
			starts = append(starts, index)
		}
	}
	ranges := make([]providerLineRange, 0, len(starts))
	for _, start := range starts {
		end := len(lines)
		for next := start + 1; next < len(lines); next++ {
			if !structural[next] {
				continue
			}
			if _, _, table := parseTOMLTable(lines[next].body); table {
				end = next
				break
			}
		}
		name, err := personEnrichmentProviderName(lines[start:end])
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, providerLineRange{start: start, end: end, name: name})
	}
	return ranges, nil
}

func personEnrichmentProviderName(block []tomlLine) (string, error) {
	structural := tomlStructuralLines(block)
	matches := make([]int, 0, 1)
	for index := 1; index < len(block); index++ {
		if !structural[index] {
			continue
		}
		key, ok := assignmentKey(block[index].body)
		if ok && equalPath(key, []string{"name"}) {
			matches = append(matches, index)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("%w: every person-enrichment provider table must contain one name",
			ErrAmbiguousConfigTarget)
	}
	end, _, _, err := assignmentSpan(block, matches[0])
	if err != nil {
		return "", fmt.Errorf("%w: invalid person-enrichment provider name", ErrAmbiguousConfigTarget)
	}
	var decoded map[string]any
	if _, err := toml.Decode(string(joinTOMLLines(block[matches[0]:end+1])), &decoded); err != nil {
		return "", fmt.Errorf("%w: invalid person-enrichment provider name", ErrAmbiguousConfigTarget)
	}
	name, ok := decoded["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%w: person-enrichment provider name is missing", ErrAmbiguousConfigTarget)
	}
	return name, nil
}

func editPersonEnrichmentProviderBlock(
	block []tomlLine,
	provider personenrichment.ProviderConfig,
) ([]tomlLine, error) {
	identifiers := make([]string, len(provider.AllowedIdentifiers))
	for index, identifier := range provider.AllowedIdentifiers {
		identifiers[index] = string(identifier)
	}
	assignments := personEnrichmentProviderAssignments(provider, identifiers, false)
	var err error
	for _, assignment := range assignments {
		block, err = editProviderBlockAssignment(block, assignment.key, assignment.value)
		if err != nil {
			return nil, fmt.Errorf("edit person-enrichment provider %q %s: %w",
				provider.Name, assignment.key, err)
		}
	}
	return block, nil
}

type providerAssignment struct {
	key   string
	value any
}

func personEnrichmentProviderAssignments(
	provider personenrichment.ProviderConfig,
	identifiers []string,
	includeIdentity bool,
) []providerAssignment {
	assignments := make([]providerAssignment, 0, 22)
	if includeIdentity {
		assignments = append(assignments,
			providerAssignment{"name", provider.Name},
			providerAssignment{"kind", provider.Kind},
			providerAssignment{"api_key_env", provider.APIKeyEnv},
		)
	}
	return append(assignments,
		providerAssignment{"enabled", provider.Enabled},
		providerAssignment{"endpoint", provider.Endpoint},
		providerAssignment{"poll_endpoint", provider.PollEndpoint},
		providerAssignment{"mode", provider.Mode},
		providerAssignment{"tier", provider.Tier},
		providerAssignment{"num_results", provider.NumResults},
		providerAssignment{"allowed_identifiers", identifiers},
		providerAssignment{"target_keys", provider.TargetKeys},
		providerAssignment{"allow_sensitive_targets", provider.AllowSensitiveTargets},
		providerAssignment{"retention_posture", provider.RetentionPosture},
		providerAssignment{"training_posture", provider.TrainingPosture},
		providerAssignment{"refresh_interval", provider.RefreshInterval.String()},
		providerAssignment{"request_timeout", provider.RequestTimeout.String()},
		providerAssignment{"poll_interval", provider.PollInterval.String()},
		providerAssignment{"max_job_age", provider.MaxJobAge.String()},
		providerAssignment{"max_retries", provider.MaxRetries},
		providerAssignment{"max_requests_per_run", provider.MaxRequestsPerRun},
		providerAssignment{"max_requests_per_day", provider.MaxRequestsPerDay},
	)
}

func editProviderBlockAssignment(block []tomlLine, key string, rawValue any) ([]tomlLine, error) {
	value, err := encodeTOMLValue(rawValue)
	if err != nil {
		return nil, err
	}
	value = strings.TrimSpace(value)
	structural := tomlStructuralLines(block)
	matches := make([]int, 0, 1)
	insertAt := 1
	for index := 1; index < len(block); index++ {
		if !structural[index] {
			continue
		}
		assignment, ok := assignmentKey(block[index].body)
		if !ok {
			continue
		}
		end, _, _, spanErr := assignmentSpan(block, index)
		if spanErr != nil {
			return nil, spanErr
		}
		if end+1 > insertAt {
			insertAt = end + 1
		}
		if equalPath(assignment, []string{key}) {
			matches = append(matches, index)
		}
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("%w: duplicate provider key %q", ErrAmbiguousConfigTarget, key)
	}
	if len(matches) == 1 {
		index := matches[0]
		end, suffix, multiline, err := assignmentSpan(block, index)
		if err != nil {
			return nil, err
		}
		replaced, err := replaceAssignmentValue(block[index].body, value)
		if err != nil {
			return nil, err
		}
		if multiline {
			replaced += suffix
			block[index].eol = block[end].eol
			block = append(block[:index+1], block[end+1:]...)
		}
		block[index].body = replaced
		return block, nil
	}

	eol := preferredEOL(block)
	if insertAt > len(block) {
		insertAt = len(block)
	}
	if insertAt > 0 && block[insertAt-1].eol == "" {
		block[insertAt-1].eol = eol
	}
	lineEOL := eol
	if insertAt == len(block) && len(block) > 0 && block[len(block)-1].eol == "" {
		lineEOL = ""
	}
	block = append(block, tomlLine{})
	copy(block[insertAt+1:], block[insertAt:])
	block[insertAt] = tomlLine{body: key + " = " + value, eol: lineEOL}
	return block, nil
}

func appendPersonEnrichmentProvider(
	lines []tomlLine,
	provider personenrichment.ProviderConfig,
) ([]tomlLine, error) {
	eol := preferredEOL(lines)
	if len(lines) > 0 {
		if lines[len(lines)-1].eol == "" {
			lines[len(lines)-1].eol = eol
		}
		if strings.TrimSpace(lines[len(lines)-1].body) != "" {
			lines = append(lines, tomlLine{eol: eol})
		}
	}
	lines = append(lines, tomlLine{body: "[[people.enrichment.providers]]", eol: eol})
	identifiers := make([]string, len(provider.AllowedIdentifiers))
	for index, identifier := range provider.AllowedIdentifiers {
		identifiers[index] = string(identifier)
	}
	for _, assignment := range personEnrichmentProviderAssignments(provider, identifiers, true) {
		value, err := encodeTOMLValue(assignment.value)
		if err != nil {
			return nil, fmt.Errorf("encode person-enrichment provider %q %s: %w",
				provider.Name, assignment.key, err)
		}
		lines = append(lines, tomlLine{body: assignment.key + " = " + strings.TrimSpace(value), eol: eol})
	}
	return lines, nil
}
