package personenrichment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

type RequestInput struct {
	PersonID          int64
	PersonRevision    int64
	Names             []IdentityCandidate
	Emails            []IdentityCandidate
	Phones            []IdentityCandidate
	CurrentCompanies  []IdentityCandidate
	PublicProfileURLs []IdentityCandidate
	Catalog           personfacts.Catalog
	Trigger           Trigger
}

type IdentityCandidate struct {
	StableID   int64
	Value      string
	Primary    bool
	ActiveFrom time.Time
}

type TriggerKind string

const (
	TriggerTracked  TriggerKind = "tracked"
	TriggerIdentity TriggerKind = "identity"
	TriggerExpiry   TriggerKind = "claim_expiry"
	TriggerRefresh  TriggerKind = "refresh"
	TriggerManual   TriggerKind = "manual"
)

type Trigger struct {
	Kind       TriggerKind
	Generation string
}

type RequestHashes struct {
	PayloadHash string
	RequestHash string
}

func BuildRequest(input RequestInput, profile ProviderProfile) (Request, RequestHashes, error) {
	if input.PersonID <= 0 {
		return Request{}, RequestHashes{}, errors.New("person ID must be positive")
	}
	targets, err := requestTargets(input.Catalog, profile)
	if err != nil {
		return Request{}, RequestHashes{}, err
	}
	identity, err := requestIdentity(input, profile.AllowedIdentifiers)
	if err != nil {
		return Request{}, RequestHashes{}, err
	}
	if profile.Kind == ProviderExa && len(identity.PublicProfileURLs) > 0 &&
		(identity.Name == "") != (identity.CurrentCompany == "") {
		identity.Name = ""
		identity.CurrentCompany = ""
	}
	if err := validateRequestIdentityForProfile(identity, profile); err != nil {
		return Request{}, RequestHashes{}, err
	}
	payloadHash, err := PayloadHash(profile.Fingerprint, identity, targets)
	if err != nil {
		return Request{}, RequestHashes{}, err
	}
	requestHash, err := RequestHash(input.PersonID, payloadHash, input.Trigger)
	if err != nil {
		return Request{}, RequestHashes{}, err
	}
	return Request{RequestHash: requestHash, Identity: identity, Targets: targets}, RequestHashes{
		PayloadHash: payloadHash, RequestHash: requestHash,
	}, nil
}

func validateRequestIdentityForProfile(identity Identity, profile ProviderProfile) error {
	switch profile.Kind {
	case ProviderExa:
		for _, profileURL := range identity.PublicProfileURLs {
			if _, err := safeExaPublicURL(profileURL); err != nil {
				return err
			}
		}
		hasPublicProfile := len(identity.PublicProfileURLs) > 0
		switch profile.Mode {
		case "people":
			if !hasPublicProfile && (identity.Name == "" || identity.CurrentCompany == "") {
				return errors.New("exa people mode requires a public profile URL or name and current company")
			}
		case "deep", "deep-reasoning":
			if !hasPublicProfile {
				return fmt.Errorf("exa %s mode requires a public profile URL", profile.Mode)
			}
		}
	case ProviderSixtyfour:
		hasName := identity.Name != ""
		hasCompany := identity.CurrentCompany != ""
		if !hasName || !hasCompany {
			return errors.New("sixtyfour requires name and current company for verified response binding")
		}
	}
	return nil
}

func requestIdentity(input RequestInput, allowed []IdentifierClass) (Identity, error) {
	identity := Identity{}
	seen := make(map[IdentifierClass]struct{}, len(allowed))
	for _, class := range allowed {
		if _, duplicate := seen[class]; duplicate {
			continue
		}
		seen[class] = struct{}{}
		switch class {
		case IdentifierName:
			identity.Name = selectIdentityCandidate(class, input.Names)
		case IdentifierEmail:
			identity.Email = selectIdentityCandidate(class, input.Emails)
		case IdentifierPhone:
			identity.Phone = selectIdentityCandidate(class, input.Phones)
		case IdentifierCurrentCompany:
			identity.CurrentCompany = selectIdentityCandidate(class, input.CurrentCompanies)
		case IdentifierPublicProfileURL:
			identity.PublicProfileURLs = selectPublicProfileURLs(input.PublicProfileURLs)
		default:
			return Identity{}, fmt.Errorf("unsupported allowed identifier class %q", class)
		}
	}
	if !hasIdentity(identity) {
		return Identity{}, errors.New("request has no eligible identity")
	}
	return identity, nil
}

type eligibleCandidate struct {
	IdentityCandidate

	normalized string
}

func selectIdentityCandidate(class IdentifierClass, candidates []IdentityCandidate) string {
	eligible := make([]eligibleCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.StableID <= 0 {
			continue
		}
		normalized, err := NormalizeIdentifier(class, candidate.Value)
		if err != nil {
			continue
		}
		eligible = append(eligible, eligibleCandidate{IdentityCandidate: candidate, normalized: normalized.Value})
	}
	if len(eligible) == 0 {
		return ""
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Primary != eligible[j].Primary {
			return eligible[i].Primary
		}
		if !eligible[i].ActiveFrom.Equal(eligible[j].ActiveFrom) {
			return eligible[i].ActiveFrom.After(eligible[j].ActiveFrom)
		}
		if eligible[i].StableID != eligible[j].StableID {
			return eligible[i].StableID < eligible[j].StableID
		}
		return eligible[i].normalized < eligible[j].normalized
	})
	return eligible[0].normalized
}

func selectPublicProfileURLs(candidates []IdentityCandidate) []string {
	unique := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.StableID <= 0 {
			continue
		}
		normalized, err := NormalizeIdentifier(IdentifierPublicProfileURL, candidate.Value)
		if err != nil {
			continue
		}
		unique[normalized.Value] = struct{}{}
	}
	if len(unique) == 0 {
		return nil
	}
	urls := make([]string, 0, len(unique))
	for value := range unique {
		urls = append(urls, value)
	}
	slices.Sort(urls)
	return urls
}

func requestTargets(catalog personfacts.Catalog, profile ProviderProfile) ([]personfacts.TargetDescriptor, error) {
	current := make(map[string]personfacts.TargetDescriptor, len(catalog.Targets))
	for _, target := range catalog.Targets {
		key := targetIdentity(target)
		if _, duplicate := current[key]; duplicate {
			return nil, fmt.Errorf("catalog contains duplicate target descriptor %q", target.Key)
		}
		current[key] = canonicalTarget(target)
	}
	if len(profile.Targets) == 0 {
		return nil, errors.New("provider profile has no target descriptors")
	}
	targets := make([]personfacts.TargetDescriptor, 0, len(profile.Targets))
	seen := make(map[string]struct{}, len(profile.Targets))
	for _, requested := range profile.Targets {
		requested = canonicalTarget(requested)
		key := targetIdentity(requested)
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("provider profile contains duplicate target descriptor %q", requested.Key)
		}
		seen[key] = struct{}{}
		if requested.Sensitive && !profile.AllowSensitiveTargets {
			return nil, fmt.Errorf("target %q is sensitive but sensitive targets are not allowed", requested.Key)
		}
		catalogTarget, ok := current[key]
		if !ok || !reflect.DeepEqual(requested, catalogTarget) {
			return nil, fmt.Errorf("target %q descriptor does not match the current catalog", requested.Key)
		}
		targets = append(targets, requested)
	}
	sortTargets(targets)
	return targets, nil
}

func targetIdentity(target personfacts.TargetDescriptor) string {
	return string(target.Kind) + "\x00" + target.Key
}

func sortTargets(targets []personfacts.TargetDescriptor) {
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Kind != targets[j].Kind {
			return targets[i].Kind < targets[j].Kind
		}
		return targets[i].Key < targets[j].Key
	})
}

func PayloadHash(
	profileFingerprint string,
	identity Identity,
	targets []personfacts.TargetDescriptor,
) (string, error) {
	if strings.TrimSpace(profileFingerprint) == "" {
		return "", errors.New("profile fingerprint must be non-empty")
	}
	canonicalIdentity, err := canonicalHashIdentity(identity)
	if err != nil {
		return "", err
	}
	canonicalTargets := cloneTargets(targets)
	sortTargets(canonicalTargets)
	encoded, err := json.Marshal(struct {
		ProfileFingerprint string                         `json:"profile_fingerprint"`
		Identity           Identity                       `json:"identity"`
		Targets            []personfacts.TargetDescriptor `json:"targets"`
	}{
		ProfileFingerprint: profileFingerprint,
		Identity:           canonicalIdentity, Targets: canonicalTargets,
	})
	if err != nil {
		return "", fmt.Errorf("encode enrichment payload hash input: %w", err)
	}
	return sha256Hex(encoded), nil
}

func canonicalHashIdentity(identity Identity) (Identity, error) {
	canonical := Identity{}
	fields := []struct {
		class IdentifierClass
		value string
		set   func(string)
	}{
		{IdentifierName, identity.Name, func(value string) { canonical.Name = value }},
		{IdentifierEmail, identity.Email, func(value string) { canonical.Email = value }},
		{IdentifierPhone, identity.Phone, func(value string) { canonical.Phone = value }},
		{IdentifierCurrentCompany, identity.CurrentCompany, func(value string) { canonical.CurrentCompany = value }},
	}
	for _, field := range fields {
		if field.value == "" {
			continue
		}
		normalized, err := NormalizeIdentifier(field.class, field.value)
		if err != nil {
			return Identity{}, err
		}
		field.set(normalized.Value)
	}
	urls := make([]IdentityCandidate, 0, len(identity.PublicProfileURLs))
	for i, value := range identity.PublicProfileURLs {
		urls = append(urls, IdentityCandidate{StableID: int64(i + 1), Value: value})
	}
	canonical.PublicProfileURLs = selectPublicProfileURLs(urls)
	if !hasIdentity(canonical) {
		return Identity{}, errors.New("payload has no eligible identity")
	}
	return canonical, nil
}

func hasIdentity(identity Identity) bool {
	return identity.Name != "" || identity.Email != "" || identity.Phone != "" ||
		identity.CurrentCompany != "" || len(identity.PublicProfileURLs) > 0
}

func RequestHash(personID int64, payloadHash string, trigger Trigger) (string, error) {
	if personID <= 0 {
		return "", errors.New("person ID must be positive")
	}
	if !isSHA256Hex(payloadHash) {
		return "", errors.New("payload hash must be 64 lowercase hexadecimal characters")
	}
	if !validTriggerKind(trigger.Kind) {
		return "", fmt.Errorf("invalid trigger kind %q", trigger.Kind)
	}
	if strings.TrimSpace(trigger.Generation) == "" {
		return "", errors.New("trigger generation must be non-empty")
	}
	encoded, err := json.Marshal(struct {
		PersonID    int64       `json:"person_id"`
		PayloadHash string      `json:"payload_hash"`
		Trigger     TriggerKind `json:"trigger_kind"`
		Generation  string      `json:"trigger_generation"`
	}{PersonID: personID, PayloadHash: payloadHash, Trigger: trigger.Kind, Generation: trigger.Generation})
	if err != nil {
		return "", fmt.Errorf("encode enrichment request hash input: %w", err)
	}
	return sha256Hex(encoded), nil
}

func validTriggerKind(kind TriggerKind) bool {
	switch kind {
	case TriggerTracked, TriggerIdentity, TriggerExpiry, TriggerRefresh, TriggerManual:
		return true
	default:
		return false
	}
}

func isSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func AssessIdentity(request Request, result Result, verified []ProviderPersonID) IdentityAssessment {
	verifiedIDs := make(map[string]struct{}, len(verified))
	for _, candidate := range verified {
		value := strings.TrimSpace(candidate.ID)
		if value != "" {
			verifiedIDs[value] = struct{}{}
		}
	}
	for _, returned := range result.ProviderPersonIDs {
		value := strings.TrimSpace(returned.ID)
		if _, ok := verifiedIDs[value]; value != "" && ok {
			return IdentityAssessment{Accepted: true, Score: 1000, Reason: "verified_provider_person_id"}
		}
	}

	strong := make(map[IdentifierClass]struct{})
	nameMatch := false
	companyMatch := false
	for _, match := range result.IdentityMatches {
		switch match.Class {
		case IdentifierEmail:
			if exactNormalizedIdentifierMatch(match.Class, request.Identity.Email, match.Value) {
				strong[match.Class] = struct{}{}
			}
		case IdentifierPhone:
			if exactNormalizedIdentifierMatch(match.Class, request.Identity.Phone, match.Value) {
				strong[match.Class] = struct{}{}
			}
		case IdentifierPublicProfileURL:
			for _, requestURL := range request.Identity.PublicProfileURLs {
				if exactNormalizedIdentifierMatch(match.Class, requestURL, match.Value) {
					strong[match.Class] = struct{}{}
					break
				}
			}
		case IdentifierName:
			nameMatch = nameMatch || exactNormalizedIdentifierMatch(match.Class, request.Identity.Name, match.Value)
		case IdentifierCurrentCompany:
			companyMatch = companyMatch || exactNormalizedIdentifierMatch(match.Class, request.Identity.CurrentCompany, match.Value)
		}
	}
	if len(strong) > 0 {
		classes := make([]IdentifierClass, 0, len(strong))
		for class := range strong {
			classes = append(classes, class)
		}
		slices.Sort(classes)
		return IdentityAssessment{
			Accepted: true, Score: 1000, Reason: "strong_identifier_match", MatchedClasses: classes,
		}
	}
	if nameMatch && companyMatch && result.IdentityConfidence >= 900 && result.IdentityConfidence <= 1000 {
		return IdentityAssessment{
			Accepted: true, Score: 900, Reason: "name_company_match",
			MatchedClasses: []IdentifierClass{IdentifierName, IdentifierCurrentCompany},
		}
	}
	return IdentityAssessment{Reason: "identity_not_verified"}
}

func exactNormalizedIdentifierMatch(class IdentifierClass, requestValue, returnedValue string) bool {
	if requestValue == "" || returnedValue == "" {
		return false
	}
	want, err := NormalizeIdentifier(class, requestValue)
	if err != nil {
		return false
	}
	got, err := NormalizeIdentifier(class, returnedValue)
	return err == nil && want.Value == got.Value
}
