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

type ProviderProfile struct {
	Fingerprint                     string                         `json:"fingerprint"`
	Name                            string                         `json:"name"`
	Kind                            string                         `json:"kind"`
	ProviderNamespace               string                         `json:"provider_namespace"`
	CatalogFingerprint              string                         `json:"catalog_fingerprint"`
	Endpoint                        string                         `json:"endpoint"`
	PollEndpoint                    string                         `json:"poll_endpoint"`
	APIKeyEnv                       string                         `json:"api_key_env"`
	Mode                            string                         `json:"mode"`
	Tier                            string                         `json:"tier"`
	NumResults                      int                            `json:"num_results"`
	AllowedIdentifiers              []IdentifierClass              `json:"allowed_identifiers"`
	Targets                         []personfacts.TargetDescriptor `json:"targets"`
	AllowSensitiveTargets           bool                           `json:"allow_sensitive_targets"`
	RetentionPosture                string                         `json:"retention_posture"`
	TrainingPosture                 string                         `json:"training_posture"`
	RefreshInterval                 time.Duration                  `json:"refresh_interval"`
	MaxRequestsPerRun               int64                          `json:"max_requests_per_run"`
	MaxRequestsPerDay               int64                          `json:"max_requests_per_day"`
	MaxCostUSDMicrosPerPersonPerDay int64                          `json:"max_cost_usd_micros_per_person_per_day"`
	MaxCostUSDMicrosPerRun          int64                          `json:"max_cost_usd_micros_per_run"`
	MaxCostUSDMicrosPerDay          int64                          `json:"max_cost_usd_micros_per_day"`
	PolicyJSON                      json.RawMessage                `json:"policy"`
}

type providerPolicy struct {
	Kind                            string                         `json:"kind"`
	ProviderNamespace               string                         `json:"provider_namespace"`
	CatalogFingerprint              string                         `json:"catalog_fingerprint"`
	Endpoint                        string                         `json:"endpoint"`
	PollEndpoint                    string                         `json:"poll_endpoint"`
	APIKeyEnv                       string                         `json:"api_key_env"`
	Mode                            string                         `json:"mode"`
	Tier                            string                         `json:"tier"`
	NumResults                      int                            `json:"num_results"`
	AllowedIdentifiers              []IdentifierClass              `json:"allowed_identifiers"`
	Targets                         []personfacts.TargetDescriptor `json:"targets"`
	AllowSensitiveTargets           bool                           `json:"allow_sensitive_targets"`
	RetentionPosture                string                         `json:"retention_posture"`
	TrainingPosture                 string                         `json:"training_posture"`
	RefreshInterval                 time.Duration                  `json:"refresh_interval"`
	MaxRequestsPerRun               int64                          `json:"max_requests_per_run"`
	MaxRequestsPerDay               int64                          `json:"max_requests_per_day"`
	MaxCostUSDMicrosPerPersonPerDay int64                          `json:"max_cost_usd_micros_per_person_per_day"`
	MaxCostUSDMicrosPerRun          int64                          `json:"max_cost_usd_micros_per_run"`
	MaxCostUSDMicrosPerDay          int64                          `json:"max_cost_usd_micros_per_day"`
}

type providerNamespaceInput struct {
	Kind         string `json:"kind"`
	Endpoint     string `json:"endpoint"`
	PollEndpoint string `json:"poll_endpoint"`
}

// ProviderNamespace returns the stable provider-identity scope without
// requiring a person-fact catalog. It is safe command metadata derived only
// from provider kind and canonical endpoints.
func (c ProviderConfig) ProviderNamespace() (string, error) {
	c.ApplyDefaults()
	if err := c.validatePolicy(false); err != nil {
		return "", err
	}
	start, err := validateHTTPSEndpoint("endpoint", c.Endpoint)
	if err != nil {
		return "", err
	}
	endpoint := canonicalEndpoint(start)
	pollEndpoint := ""
	if c.PollEndpoint != "" {
		poll, pollErr := validateHTTPSEndpoint("poll_endpoint", c.PollEndpoint)
		if pollErr != nil {
			return "", pollErr
		}
		pollEndpoint = canonicalEndpoint(poll)
	}
	encoded, err := json.Marshal(providerNamespaceInput{
		Kind: c.Kind, Endpoint: endpoint, PollEndpoint: pollEndpoint,
	})
	if err != nil {
		return "", fmt.Errorf("encode provider namespace: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return c.Kind + ":" + hex.EncodeToString(digest[:]), nil
}

// Validate reconstructs the canonical profile from its complete immutable
// policy and requires every public field to match. This is the inverse of
// ProviderConfig.Profile and is the boundary durable consent uses before it
// accepts a caller-supplied profile.
func (p ProviderProfile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("provider profile name is required")
	}
	if !json.Valid(p.PolicyJSON) {
		return errors.New("provider profile policy is not valid JSON")
	}
	digest := sha256.Sum256(p.PolicyJSON)
	if hex.EncodeToString(digest[:]) != p.Fingerprint {
		return errors.New("provider profile policy does not match its fingerprint")
	}

	var policy providerPolicy
	if err := json.Unmarshal(p.PolicyJSON, &policy); err != nil {
		return fmt.Errorf("decode provider profile policy: %w", err)
	}
	targetKeys := make([]string, len(policy.Targets))
	for i := range policy.Targets {
		targetKeys[i] = policy.Targets[i].Key
	}
	canonical, err := (ProviderConfig{
		Name:                            p.Name,
		Kind:                            policy.Kind,
		Enabled:                         true,
		Endpoint:                        policy.Endpoint,
		PollEndpoint:                    policy.PollEndpoint,
		APIKeyEnv:                       policy.APIKeyEnv,
		Mode:                            policy.Mode,
		Tier:                            policy.Tier,
		NumResults:                      policy.NumResults,
		AllowedIdentifiers:              slices.Clone(policy.AllowedIdentifiers),
		TargetKeys:                      targetKeys,
		AllowSensitiveTargets:           policy.AllowSensitiveTargets,
		RetentionPosture:                policy.RetentionPosture,
		TrainingPosture:                 policy.TrainingPosture,
		RefreshInterval:                 policy.RefreshInterval,
		RequestTimeout:                  time.Second,
		PollInterval:                    time.Second,
		MaxJobAge:                       time.Second,
		MaxRequestsPerRun:               policy.MaxRequestsPerRun,
		MaxRequestsPerDay:               policy.MaxRequestsPerDay,
		MaxCostUSDMicrosPerPersonPerDay: policy.MaxCostUSDMicrosPerPersonPerDay,
		MaxCostUSDMicrosPerRun:          policy.MaxCostUSDMicrosPerRun,
		MaxCostUSDMicrosPerDay:          policy.MaxCostUSDMicrosPerDay,
	}).Profile(personfacts.Catalog{Targets: cloneTargets(policy.Targets)})
	if err != nil {
		return fmt.Errorf("validate provider profile policy: %w", err)
	}
	if !reflect.DeepEqual(p, canonical) {
		return errors.New("provider profile does not match its canonical immutable policy")
	}
	return nil
}

func (c ProviderConfig) Profile(catalog personfacts.Catalog) (ProviderProfile, error) {
	if err := c.validatePolicy(false); err != nil {
		return ProviderProfile{}, err
	}
	start, err := validateHTTPSEndpoint("endpoint", c.Endpoint)
	if err != nil {
		return ProviderProfile{}, err
	}
	endpoint := canonicalEndpoint(start)
	pollEndpoint := ""
	if c.PollEndpoint != "" {
		poll, pollErr := validateHTTPSEndpoint("poll_endpoint", c.PollEndpoint)
		if pollErr != nil {
			return ProviderProfile{}, pollErr
		}
		pollEndpoint = canonicalEndpoint(poll)
	}

	targets, err := expandTargets(c.TargetKeys, catalog, c.AllowSensitiveTargets)
	if err != nil {
		return ProviderProfile{}, err
	}
	if c.Kind == ProviderExa && c.Mode == "people" {
		for _, target := range targets {
			if _, err := exaPeopleCapabilityForTarget(target); err != nil {
				return ProviderProfile{}, fmt.Errorf("validate Exa people target %q: %w", target.Key, err)
			}
		}
	}
	if c.Kind == ProviderSixtyfour {
		for _, target := range targets {
			if _, err := sixtyfourStructValue(target); err != nil {
				return ProviderProfile{}, fmt.Errorf(
					"validate Sixtyfour target %q: %w", target.Key, err)
			}
		}
	}
	catalogFingerprint, err := personfacts.CatalogFingerprint(targets)
	if err != nil {
		return ProviderProfile{}, fmt.Errorf("fingerprint provider target catalog: %w", err)
	}
	identifiers := slices.Clone(c.AllowedIdentifiers)
	slices.Sort(identifiers)
	namespaceInput, err := json.Marshal(providerNamespaceInput{
		Kind: c.Kind, Endpoint: endpoint, PollEndpoint: pollEndpoint,
	})
	if err != nil {
		return ProviderProfile{}, fmt.Errorf("encode provider namespace: %w", err)
	}
	namespaceDigest := sha256.Sum256(namespaceInput)
	providerNamespace := c.Kind + ":" + hex.EncodeToString(namespaceDigest[:])

	policy := providerPolicy{
		Kind: c.Kind, ProviderNamespace: providerNamespace,
		CatalogFingerprint: catalogFingerprint, Endpoint: endpoint,
		PollEndpoint: pollEndpoint, APIKeyEnv: c.APIKeyEnv, Mode: c.Mode,
		Tier: c.Tier, NumResults: c.NumResults, AllowedIdentifiers: identifiers,
		Targets: targets, AllowSensitiveTargets: c.AllowSensitiveTargets,
		RetentionPosture: c.RetentionPosture, TrainingPosture: c.TrainingPosture,
		RefreshInterval: c.RefreshInterval, MaxRequestsPerRun: c.MaxRequestsPerRun,
		MaxRequestsPerDay:               c.MaxRequestsPerDay,
		MaxCostUSDMicrosPerPersonPerDay: c.MaxCostUSDMicrosPerPersonPerDay,
		MaxCostUSDMicrosPerRun:          c.MaxCostUSDMicrosPerRun,
		MaxCostUSDMicrosPerDay:          c.MaxCostUSDMicrosPerDay,
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return ProviderProfile{}, fmt.Errorf("encode provider policy: %w", err)
	}
	digest := sha256.Sum256(policyJSON)
	return ProviderProfile{
		Fingerprint: hex.EncodeToString(digest[:]), Name: c.Name, Kind: c.Kind,
		ProviderNamespace: providerNamespace, CatalogFingerprint: catalogFingerprint,
		Endpoint: endpoint, PollEndpoint: pollEndpoint, APIKeyEnv: c.APIKeyEnv,
		Mode: c.Mode, Tier: c.Tier, NumResults: c.NumResults,
		AllowedIdentifiers: slices.Clone(identifiers), Targets: cloneTargets(targets),
		AllowSensitiveTargets: c.AllowSensitiveTargets,
		RetentionPosture:      c.RetentionPosture, TrainingPosture: c.TrainingPosture,
		RefreshInterval: c.RefreshInterval, MaxRequestsPerRun: c.MaxRequestsPerRun,
		MaxRequestsPerDay:               c.MaxRequestsPerDay,
		MaxCostUSDMicrosPerPersonPerDay: c.MaxCostUSDMicrosPerPersonPerDay,
		MaxCostUSDMicrosPerRun:          c.MaxCostUSDMicrosPerRun,
		MaxCostUSDMicrosPerDay:          c.MaxCostUSDMicrosPerDay,
		PolicyJSON:                      append(json.RawMessage(nil), policyJSON...),
	}, nil
}

func expandTargets(
	keys []string,
	catalog personfacts.Catalog,
	allowSensitive bool,
) ([]personfacts.TargetDescriptor, error) {
	byKey := make(map[string]personfacts.TargetDescriptor, len(catalog.Targets))
	for _, target := range catalog.Targets {
		if _, exists := byKey[target.Key]; exists {
			return nil, fmt.Errorf("catalog contains duplicate target %q", target.Key)
		}
		byKey[target.Key] = target
	}
	targets := make([]personfacts.TargetDescriptor, 0, len(keys))
	for _, key := range keys {
		target, ok := byKey[key]
		if !ok {
			return nil, fmt.Errorf("target %q is not present in catalog", key)
		}
		if target.Sensitive && !allowSensitive {
			return nil, fmt.Errorf("target %q is sensitive but sensitive targets are not allowed", key)
		}
		if target.Revision == "" {
			return nil, fmt.Errorf("target %q has no descriptor revision", key)
		}
		targets = append(targets, canonicalTarget(target))
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Kind != targets[j].Kind {
			return targets[i].Kind < targets[j].Kind
		}
		return targets[i].Key < targets[j].Key
	})
	return targets, nil
}

func canonicalTarget(target personfacts.TargetDescriptor) personfacts.TargetDescriptor {
	target.Choices = slices.Clone(target.Choices)
	sort.Slice(target.Choices, func(i, j int) bool {
		if target.Choices[i].Value != target.Choices[j].Value {
			return target.Choices[i].Value < target.Choices[j].Value
		}
		return target.Choices[i].Label < target.Choices[j].Label
	})
	target.Fields = slices.Clone(target.Fields)
	sort.Slice(target.Fields, func(i, j int) bool {
		return target.Fields[i].Name < target.Fields[j].Name
	})
	return target
}

func cloneTargets(targets []personfacts.TargetDescriptor) []personfacts.TargetDescriptor {
	cloned := make([]personfacts.TargetDescriptor, len(targets))
	for i := range targets {
		cloned[i] = canonicalTarget(targets[i])
	}
	return cloned
}
