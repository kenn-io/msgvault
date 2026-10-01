package mcp

import (
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"go.kenn.io/msgvault/internal/store"
)

// DocumentPolicyDetails is the production profile's non-secret canonical policy.
// IncludeInline follows its owning optional producer; no MCP option changes it.
type DocumentPolicyDetails struct {
	Version                   int                    `json:"version"`
	Provider                  string                 `json:"provider"`
	Endpoint                  string                 `json:"endpoint"`
	Model                     string                 `json:"model"`
	Retention                 string                 `json:"retention"`
	Training                  string                 `json:"training"`
	MaxFileBytes              int64                  `json:"max_file_bytes"`
	MaxPagesPerDocument       int                    `json:"max_pages_per_document"`
	MaxResponseBytes          int64                  `json:"max_response_bytes"`
	MaxNormalizedChars        int                    `json:"max_normalized_chars"`
	MaxSpoolBytes             int64                  `json:"max_spool_bytes"`
	MinFreeSpaceBytes         int64                  `json:"min_free_space_bytes"`
	RequestTimeoutNanos       int64                  `json:"request_timeout_nanos"`
	MaxRetries                int                    `json:"max_retries"`
	MaxPagesPerRun            int                    `json:"max_pages_per_run"`
	MaxEstimatedCostUSDPerRun float64                `json:"max_estimated_cost_usd_per_run"`
	MessageTypes              []string               `json:"message_types"`
	AllowedMediaTypes         []string               `json:"allowed_media_types"`
	IncludeInline             bool                   `json:"include_inline,omitempty"`
	DocumentPolicyFingerprint string                 `json:"document_policy_fingerprint"`
	Lexical                   bool                   `json:"lexical"`
	StoreChunkText            bool                   `json:"store_chunk_text"`
	ExtractHeader             bool                   `json:"extract_header"`
	ExtractFooter             bool                   `json:"extract_footer"`
	NormalizationVersion      int                    `json:"normalization_version"`
	MaxUnitChars              int                    `json:"max_unit_chars"`
	MaxSourceUnitBytes        int                    `json:"max_source_unit_bytes"`
	MaxMetadataSourceBytes    int                    `json:"max_metadata_source_bytes"`
	MaxLinkChars              int                    `json:"max_link_chars"`
	MaxChunkRunes             int                    `json:"max_chunk_runes"`
	ChunkOverlap              int                    `json:"chunk_overlap"`
	MaxChunks                 int                    `json:"max_chunks"`
	CSVConversion             *DocumentCSVConversion `json:"csv_conversion,omitempty"`
}
type DocumentCSVConversion struct {
	SourceMediaType   string `json:"source_media_type"`
	ProviderMediaType string `json:"provider_media_type"`
	PolicyFingerprint string `json:"policy_fingerprint"`
	ConverterVersion  string `json:"converter_version"`
}
type DocumentPolicy struct {
	ProfileID      string                `json:"profile_id"`
	Fingerprint    string                `json:"fingerprint"`
	Enabled        bool                  `json:"enabled"`
	ProfileExists  bool                  `json:"profile_exists"`
	ProfileEnabled bool                  `json:"profile_enabled"`
	Region         string                `json:"region"`
	Policy         DocumentPolicyDetails `json:"policy"`
	Disclosure     string                `json:"disclosure"`
	ExactConsent   bool                  `json:"exact_consent"`
	ConsentedAt    *time.Time            `json:"consented_at,omitempty"`
}
type DocumentConsent struct {
	ProfileID    string     `json:"profile_id"`
	Fingerprint  string     `json:"fingerprint"`
	ExactConsent bool       `json:"exact_consent"`
	ConsentedAt  *time.Time `json:"consented_at,omitempty"`
}
type DocumentFailure struct {
	CanonicalBlobHash string `json:"canonical_blob_hash"`
	ReasonCode        string `json:"reason_code"`
	Detail            string `json:"detail,omitempty"`
	State             string `json:"state,omitempty"`
}
type DocumentCoverage struct {
	store.DocumentIndexStatus

	Failures          []DocumentFailure `json:"failures,omitempty"`
	FailuresExhausted *bool             `json:"failures_exhausted,omitempty"`
}
type DocumentIndexStatus struct {
	Status        DocumentCoverage                  `json:"status"`
	ActiveRebuild *store.DocumentIndexRebuildStatus `json:"active_rebuild,omitempty"`
}
type DocumentBuild struct {
	ProfileID         string            `json:"profile_id"`
	Fingerprint       string            `json:"fingerprint"`
	RunID             int64             `json:"run_id"`
	State             string            `json:"state"`
	Attempted         int               `json:"attempted"`
	Succeeded         int               `json:"succeeded"`
	Failed            int               `json:"failed"`
	Skipped           int               `json:"skipped"`
	Units             int               `json:"units"`
	Reconciled        int               `json:"reconciled"`
	Changes           int               `json:"changes"`
	CleanupFailures   int               `json:"cleanup_failures"`
	RebuildID         string            `json:"rebuild_id,omitempty"`
	Remaining         int64             `json:"remaining"`
	Completed         bool              `json:"completed"`
	Failures          []DocumentFailure `json:"failures"`
	FailuresExhausted bool              `json:"failures_exhausted"`
	Coverage          DocumentCoverage  `json:"coverage"`
}
type DocumentRetry struct {
	ProfileID         string `json:"profile_id"`
	Fingerprint       string `json:"fingerprint"`
	CanonicalBlobHash string `json:"canonical_blob_hash"`
	Reset             bool   `json:"reset"`
}

func documentOperationalDefinitions() []operationalDefinition {
	defs := []operationalDefinition{
		newOperationalDefinition("get_document_index_status", "Read the daemon's durable document coverage, exact consent, rebuild progress and bounded safe failure diagnostics. Works without a local manifest.", OperationFamilyDocuments, closedObject(nil), outputSchemaFor[DocumentIndexStatus](), false, false),
		newOperationalDefinition("get_document_processing_policy", "Preview the exact authenticated document upload policy selected by the host operator, current fingerprint and consent. Does not record consent or contact the provider.", OperationFamilyDocuments, closedObject(nil), outputSchemaFor[DocumentPolicy](), false, false),
		newOperationalDefinition("consent_document_processing", "Record exact document upload consent after reviewing the current policy and approving this call. No arbitrary manifest path or inline-scope override.", OperationFamilyDocuments, closedObject(map[string]*jsonschema.Schema{"fingerprint": stringSchema("Exact current processing policy fingerprint")}, "fingerprint"), outputSchemaFor[DocumentConsent](), true, false),
	}
	for _, name := range []string{"build_document_index", "resume_document_index"} {
		props := map[string]*jsonschema.Schema{"fingerprint": stringSchema("Exact current processing policy fingerprint"), toolArgLimit: {Type: mcpSchemaInteger, Minimum: new(float64(1)), Maximum: new(float64(10000)), Description: "Maximum documents; defaults to 100, constrained by the host run budget"}}
		if name == "build_document_index" {
			props["full_rebuild"] = &jsonschema.Schema{Type: mcpSchemaBoolean, Description: "Start a replacement rebuild; resume preserves its existing identity"}
		}
		defs = append(defs, newOperationalDefinition(name, "Run an approved hosted document extraction pass under exact consent. Uploads may incur charges. Preserve partial failures and rebuild continuation; never retries an uncertain call.", OperationFamilyDocuments, closedObject(props, "fingerprint"), outputSchemaFor[DocumentBuild](), true, false))
	}
	return append(defs, newOperationalDefinition("retry_document_extraction", "Reset failed work for one exact lowercase SHA-256 under the current policy after approval. This schedules work; it does not upload or complete an extraction.", OperationFamilyDocuments, closedObject(map[string]*jsonschema.Schema{"fingerprint": stringSchema("Exact current processing policy fingerprint"), "canonical_blob_hash": stringSchema("Exact lowercase attachment SHA-256")}, "fingerprint", "canonical_blob_hash"), outputSchemaFor[DocumentRetry](), true, false))
}
