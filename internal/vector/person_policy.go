package vector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

const (
	// SemanticPersonEmbeddingPurpose is deliberately distinct from the
	// people-sweep inference purpose. Consent to either policy cannot authorize
	// the other.
	SemanticPersonEmbeddingPurpose = "semantic_person_embeddings"
	// SemanticPersonCorpusAllDurablePeople records that message embedding scope
	// is not applicable to curated people.
	SemanticPersonCorpusAllDurablePeople = "all_durable_people"
	// SemanticPersonRendererPolicy changes whenever the canonical curated
	// person document changes.
	SemanticPersonRendererPolicy = "person-semantic-v1"
	// SemanticPersonSearchQueryDisclosedFieldClass records that caller-supplied
	// free text is sent to the provider for semantic person search.
	SemanticPersonSearchQueryDisclosedFieldClass = "caller_supplied_free_text_query_for_semantic_person_search"
)

var semanticPersonEnvironmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var semanticPersonDisclosedFieldClasses = []string{
	"active_relationship_counterpart_labels_and_display_names",
	SemanticPersonSearchQueryDisclosedFieldClass,
	"current_employment_title_role_department_location_description",
	"current_organization_alternate_names_categories_description_domain_kind",
	"current_organization_coarse_locations",
	"current_organization_name",
	"person_alternate_names",
	"person_categories",
	"person_coarse_locations",
	"person_display_name",
	"person_searchable_non_sensitive_custom_attributes_excluding_email_phone_date_timestamp",
}

// PeopleConfig is the separate, default-off policy switch for curated person
// embeddings under [vector.people]. Provider posture assertions are part of
// exact consent rather than operational embedding settings.
type PeopleConfig struct {
	Enabled          bool   `toml:"enabled"`
	RetentionPosture string `toml:"retention_posture"`
	TrainingPosture  string `toml:"training_posture"`
}

// SemanticPersonEmbeddingProfile is one immutable, canonical outbound-data
// policy. PolicyJSON and Fingerprint never contain a credential value.
type SemanticPersonEmbeddingProfile struct {
	Fingerprint           string             `json:"fingerprint"`
	Purpose               string             `json:"purpose"`
	Destination           string             `json:"destination"`
	APIFormat             EmbeddingAPIFormat `json:"api_format"`
	Model                 string             `json:"model"`
	APIKeyEnv             string             `json:"api_key_env"`
	RetentionPosture      string             `json:"retention_posture"`
	TrainingPosture       string             `json:"training_posture"`
	RendererPolicy        string             `json:"renderer_policy"`
	DisclosedFieldClasses []string           `json:"disclosed_field_classes"`
	CorpusScope           string             `json:"corpus_scope"`
	PolicyJSON            json.RawMessage    `json:"-"`
}

type semanticPersonEmbeddingPolicy struct {
	Purpose               string             `json:"purpose"`
	Destination           string             `json:"destination"`
	APIFormat             EmbeddingAPIFormat `json:"api_format"`
	Model                 string             `json:"model"`
	APIKeyEnv             string             `json:"api_key_env"`
	RetentionPosture      string             `json:"retention_posture"`
	TrainingPosture       string             `json:"training_posture"`
	RendererPolicy        string             `json:"renderer_policy"`
	DisclosedFieldClasses []string           `json:"disclosed_field_classes"`
	CorpusScope           string             `json:"corpus_scope"`
}

// SemanticPersonDisclosedFieldClasses returns the stable explicit outbound
// data-class disclosure in canonical order.
func SemanticPersonDisclosedFieldClasses() []string {
	return slices.Clone(semanticPersonDisclosedFieldClasses)
}

// SemanticPersonEmbeddingProfile returns the exact current curated-person
// egress policy. Enablement is checked separately by the runtime gate so a
// disabled but still fully configured policy remains auditable and revocable.
func (c *Config) SemanticPersonEmbeddingProfile() (SemanticPersonEmbeddingProfile, error) {
	policy, err := semanticPersonEmbeddingPolicyForConfig(*c)
	if err != nil {
		return SemanticPersonEmbeddingProfile{}, err
	}
	return newSemanticPersonEmbeddingProfile(policy)
}

func semanticPersonEmbeddingPolicyForConfig(c Config) (semanticPersonEmbeddingPolicy, error) {
	format := c.Embeddings.EffectiveAPIFormat()
	endpoint := c.Embeddings.Endpoint
	if endpoint != strings.TrimSpace(endpoint) {
		return semanticPersonEmbeddingPolicy{}, errors.New(
			"vector.people: embedding endpoint must not have surrounding whitespace",
		)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return semanticPersonEmbeddingPolicy{}, fmt.Errorf(
			"vector.people: embedding destination must be an absolute HTTP(S) URL (got %q)",
			c.Embeddings.Endpoint,
		)
	}
	if parsed.User != nil {
		return semanticPersonEmbeddingPolicy{}, errors.New(
			"vector.people: embedding destination must not contain credentials",
		)
	}
	if parsed.RawQuery != "" {
		return semanticPersonEmbeddingPolicy{}, errors.New(
			"vector.people: embedding endpoint must not contain a query",
		)
	}
	if parsed.Fragment != "" {
		return semanticPersonEmbeddingPolicy{}, errors.New(
			"vector.people: embedding endpoint must not contain a fragment",
		)
	}
	model := c.Embeddings.Model
	if model != strings.TrimSpace(model) {
		return semanticPersonEmbeddingPolicy{}, errors.New(
			"vector.people: embedding model must not have surrounding whitespace",
		)
	}
	apiKeyEnv := c.Embeddings.APIKeyEnv
	if apiKeyEnv != strings.TrimSpace(apiKeyEnv) {
		return semanticPersonEmbeddingPolicy{}, errors.New(
			"vector.people: embedding api_key_env must not have surrounding whitespace",
		)
	}
	destination := endpoint + "/embeddings"
	if format == APIFormatVoyageContextual {
		destination = strings.TrimRight(endpoint, "/") + "/contextualizedembeddings"
	}
	return semanticPersonEmbeddingPolicy{
		Purpose:               SemanticPersonEmbeddingPurpose,
		Destination:           destination,
		APIFormat:             format,
		Model:                 model,
		APIKeyEnv:             apiKeyEnv,
		RetentionPosture:      strings.TrimSpace(c.People.RetentionPosture),
		TrainingPosture:       strings.TrimSpace(c.People.TrainingPosture),
		RendererPolicy:        SemanticPersonRendererPolicy,
		DisclosedFieldClasses: SemanticPersonDisclosedFieldClasses(),
		CorpusScope:           SemanticPersonCorpusAllDurablePeople,
	}, nil
}

func newSemanticPersonEmbeddingProfile(
	policy semanticPersonEmbeddingPolicy,
) (SemanticPersonEmbeddingProfile, error) {
	policy.Purpose = strings.TrimSpace(policy.Purpose)
	policy.Destination = strings.TrimSpace(policy.Destination)
	policy.Model = strings.TrimSpace(policy.Model)
	policy.APIKeyEnv = strings.TrimSpace(policy.APIKeyEnv)
	policy.RetentionPosture = strings.TrimSpace(policy.RetentionPosture)
	policy.TrainingPosture = strings.TrimSpace(policy.TrainingPosture)
	policy.RendererPolicy = strings.TrimSpace(policy.RendererPolicy)
	policy.CorpusScope = strings.TrimSpace(policy.CorpusScope)
	policy.DisclosedFieldClasses = slices.Clone(policy.DisclosedFieldClasses)
	slices.Sort(policy.DisclosedFieldClasses)
	if err := validateSemanticPersonEmbeddingPolicy(policy); err != nil {
		return SemanticPersonEmbeddingProfile{}, err
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return SemanticPersonEmbeddingProfile{}, fmt.Errorf(
			"encode semantic person embedding policy: %w", err,
		)
	}
	digest := sha256.Sum256(policyJSON)
	return SemanticPersonEmbeddingProfile{
		Fingerprint:           hex.EncodeToString(digest[:]),
		Purpose:               policy.Purpose,
		Destination:           policy.Destination,
		APIFormat:             policy.APIFormat,
		Model:                 policy.Model,
		APIKeyEnv:             policy.APIKeyEnv,
		RetentionPosture:      policy.RetentionPosture,
		TrainingPosture:       policy.TrainingPosture,
		RendererPolicy:        policy.RendererPolicy,
		DisclosedFieldClasses: slices.Clone(policy.DisclosedFieldClasses),
		CorpusScope:           policy.CorpusScope,
		PolicyJSON:            policyJSON,
	}, nil
}

func validateSemanticPersonEmbeddingPolicy(policy semanticPersonEmbeddingPolicy) error {
	if policy.Purpose == "" {
		return errors.New("vector.people: semantic embedding purpose is required")
	}
	parsed, err := url.Parse(policy.Destination)
	if err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("vector.people: effective embedding destination is invalid")
	}
	if parsed.User != nil {
		return errors.New("vector.people: effective embedding destination must not contain credentials")
	}
	if parsed.RawQuery != "" {
		return errors.New("vector.people: effective embedding destination must not contain a query")
	}
	if parsed.Fragment != "" {
		return errors.New("vector.people: effective embedding destination must not contain a fragment")
	}
	switch policy.APIFormat {
	case APIFormatOpenAI, APIFormatVoyageContextual:
	default:
		return fmt.Errorf("vector.people: embedding API format %q is invalid", policy.APIFormat)
	}
	if policy.Model == "" {
		return errors.New("vector.people: embedding model is required")
	}
	if policy.APIKeyEnv != "" && !semanticPersonEnvironmentNamePattern.MatchString(policy.APIKeyEnv) {
		return fmt.Errorf("vector.people: embedding api_key_env %q is invalid", policy.APIKeyEnv)
	}
	if policy.RetentionPosture == "" || strings.EqualFold(policy.RetentionPosture, "unknown") {
		return errors.New("vector.people: retention_posture must be explicit")
	}
	if policy.TrainingPosture == "" || strings.EqualFold(policy.TrainingPosture, "unknown") {
		return errors.New("vector.people: training_posture must be explicit")
	}
	if policy.RendererPolicy == "" {
		return errors.New("vector.people: renderer policy is required")
	}
	if len(policy.DisclosedFieldClasses) == 0 {
		return errors.New("vector.people: disclosed field classes must not be empty")
	}
	for i, field := range policy.DisclosedFieldClasses {
		if strings.TrimSpace(field) == "" || field != strings.TrimSpace(field) {
			return errors.New("vector.people: disclosed field classes must be non-empty canonical strings")
		}
		if i > 0 && field == policy.DisclosedFieldClasses[i-1] {
			return fmt.Errorf("vector.people: duplicate disclosed field class %q", field)
		}
	}
	if policy.CorpusScope == "" {
		return errors.New("vector.people: corpus scope is required")
	}
	return nil
}

// Validate proves that the public fields, canonical policy bytes, and
// fingerprint still describe exactly one immutable policy.
func (p SemanticPersonEmbeddingProfile) Validate() error {
	_, err := p.Canonical()
	return err
}

// Canonical validates a possibly database-normalized profile and returns its
// canonical policy JSON. PostgreSQL JSONB is allowed to normalize whitespace
// and key ordering, but never policy content.
func (p SemanticPersonEmbeddingProfile) Canonical() (SemanticPersonEmbeddingProfile, error) {
	want, err := newSemanticPersonEmbeddingProfile(semanticPersonEmbeddingPolicy{
		Purpose: p.Purpose, Destination: p.Destination, APIFormat: p.APIFormat,
		Model: p.Model, APIKeyEnv: p.APIKeyEnv,
		RetentionPosture: p.RetentionPosture, TrainingPosture: p.TrainingPosture,
		RendererPolicy:        p.RendererPolicy,
		DisclosedFieldClasses: slices.Clone(p.DisclosedFieldClasses),
		CorpusScope:           p.CorpusScope,
	})
	if err != nil {
		return SemanticPersonEmbeddingProfile{}, err
	}
	if p.Fingerprint != want.Fingerprint {
		return SemanticPersonEmbeddingProfile{}, errors.New("semantic person embedding profile fingerprint does not match policy")
	}
	if !semanticPersonJSONEqual(p.PolicyJSON, want.PolicyJSON) {
		return SemanticPersonEmbeddingProfile{}, errors.New("semantic person embedding profile policy does not match immutable fields")
	}
	if p.Purpose != want.Purpose || p.Destination != want.Destination ||
		p.APIFormat != want.APIFormat || p.Model != want.Model ||
		p.APIKeyEnv != want.APIKeyEnv || p.RetentionPosture != want.RetentionPosture ||
		p.TrainingPosture != want.TrainingPosture ||
		p.RendererPolicy != want.RendererPolicy ||
		!slices.Equal(p.DisclosedFieldClasses, want.DisclosedFieldClasses) ||
		p.CorpusScope != want.CorpusScope {
		return SemanticPersonEmbeddingProfile{}, errors.New("semantic person embedding profile fields are not canonical")
	}
	return want, nil
}

func semanticPersonJSONEqual(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}
