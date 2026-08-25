package personfacts

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var lowercaseSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type evidenceValidationError struct {
	reason DecisionReason
	detail string
}

func (e *evidenceValidationError) Error() string { return e.detail }

// EvidenceKey validates an immutable evidence version and hashes every field
// that identifies or interprets that version.
func EvidenceKey(input EvidenceInput) (string, error) {
	if err := validateEvidenceInput(input); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(evidenceKeyView(input))
	if err != nil {
		return "", fmt.Errorf("encode evidence key: %w", err)
	}
	return fingerprint(encoded), nil
}

func validateEvidenceInput(input EvidenceInput) error {
	invalid := func(reason DecisionReason, format string, args ...any) error {
		return &evidenceValidationError{reason: reason, detail: fmt.Sprintf(format, args...)}
	}
	if input.PersonID <= 0 {
		return invalid(ReasonMalformedValue, "evidence person id must be positive")
	}
	if input.SubjectPersonID == nil || *input.SubjectPersonID != input.PersonID {
		return invalid(ReasonIdentityMismatch, "evidence subject person id must equal evidence person id")
	}
	if !validEvidenceSourceClass(input.SourceClass) {
		return invalid(ReasonMalformedValue, "unknown evidence source class %q", input.SourceClass)
	}
	if !validEvidenceDirectness(input.Directness) {
		return invalid(ReasonMalformedValue, "unknown evidence directness %q", input.Directness)
	}
	if !validEvidenceAuthority(input.Authority) {
		return invalid(ReasonMalformedValue, "unknown evidence authority %q", input.Authority)
	}
	if utf8.RuneCountInString(input.Excerpt) > MaxEvidenceExcerptRunes {
		return invalid(ReasonMalformedValue, "evidence excerpt exceeds %d Unicode characters", MaxEvidenceExcerptRunes)
	}
	if input.IdentityScore < 0 || input.IdentityScore > 1000 {
		return invalid(ReasonMalformedValue, "evidence identity score must be between 0 and 1000")
	}
	if input.EventTime.IsZero() || !isUTC(input.EventTime) {
		return invalid(ReasonMalformedValue, "evidence event time must be nonzero UTC")
	}
	if input.RecordedTime.IsZero() || !isUTC(input.RecordedTime) {
		return invalid(ReasonMalformedValue, "evidence recorded time must be nonzero UTC")
	}
	if input.ContentSHA256 != "" && !lowercaseSHA256Pattern.MatchString(input.ContentSHA256) {
		return invalid(ReasonMalformedValue, "evidence content hash must be lowercase 64-hex SHA-256")
	}

	pairedSpan := input.SpanStart != nil && input.SpanEnd != nil
	if (input.SpanStart == nil) != (input.SpanEnd == nil) {
		return invalid(ReasonMalformedValue, "evidence span start and end must be paired")
	}
	if pairedSpan && (*input.SpanStart < 0 || *input.SpanEnd < *input.SpanStart) {
		return invalid(ReasonMalformedValue, "evidence span must be nonnegative and ordered")
	}

	switch input.SourceClass {
	case EvidenceArchive:
		if strings.TrimSpace(input.SourceRef) == "" {
			return invalid(ReasonMalformedValue, "archive evidence source ref is required")
		}
		if !pairedSpan {
			return invalid(ReasonMalformedValue, "archive evidence requires a source span")
		}
		if input.SourceVersion == "" {
			return invalid(ReasonMalformedValue, "archive evidence source version is required")
		}
		if !lowercaseSHA256Pattern.MatchString(input.ContentSHA256) {
			return invalid(ReasonMalformedValue, "archive evidence content hash must be lowercase 64-hex SHA-256")
		}
		if input.SourceURL != "" && !validHTTPSURL(input.SourceURL) {
			return invalid(ReasonMalformedValue, "archive evidence source URL must use HTTPS")
		}
	case EvidencePublic:
		if !validHTTPSURL(input.SourceURL) {
			return invalid(ReasonMalformedValue, "public evidence requires an HTTPS source URL")
		}
		if pairedSpan {
			return invalid(ReasonMalformedValue, "public evidence forbids archive spans")
		}
	case EvidenceProviderAssertion:
		if strings.TrimSpace(input.SourceRef) == "" {
			return invalid(ReasonMalformedValue, "provider assertion requires an opaque source ref")
		}
		if pairedSpan {
			return invalid(ReasonMalformedValue, "provider assertion forbids archive spans")
		}
		if input.SourceURL != "" && !validHTTPSURL(input.SourceURL) {
			return invalid(ReasonMalformedValue, "provider assertion source URL must use HTTPS")
		}
		if input.Directness != Indirect {
			return invalid(ReasonMalformedValue, "provider assertion directness must be indirect")
		}
		if input.Authority != AuthorityOrdinary && input.Authority != AuthorityAggregator {
			return invalid(ReasonMalformedValue, "provider assertion authority must be ordinary or aggregator")
		}
	case EvidenceSystem:
		if strings.TrimSpace(input.SourceRef) == "" {
			return invalid(ReasonMalformedValue, "system evidence source ref is required")
		}
		if pairedSpan {
			return invalid(ReasonMalformedValue, "system evidence forbids archive spans")
		}
		if input.SourceURL != "" && !validHTTPSURL(input.SourceURL) {
			return invalid(ReasonMalformedValue, "system evidence source URL must use HTTPS")
		}
	}
	return nil
}

func validEvidenceSourceClass(value EvidenceSourceClass) bool {
	return value == EvidenceArchive || value == EvidencePublic ||
		value == EvidenceSystem || value == EvidenceProviderAssertion
}

func validEvidenceDirectness(value EvidenceDirectness) bool {
	return value == DirectSelf || value == DirectOther || value == Indirect
}

func validEvidenceAuthority(value EvidenceAuthority) bool {
	return value == AuthorityAuthoritative || value == AuthorityOrdinary || value == AuthorityAggregator
}

func validHTTPSURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil
}

func isUTC(value time.Time) bool {
	_, offset := value.Zone()
	return offset == 0
}

type canonicalEvidence struct {
	PersonID        int64               `json:"person_id"`
	SourceClass     EvidenceSourceClass `json:"source_class"`
	Directness      EvidenceDirectness  `json:"directness"`
	Authority       EvidenceAuthority   `json:"authority"`
	SourceRef       string              `json:"source_ref"`
	SourceURL       string              `json:"source_url"`
	SubjectPersonID *int64              `json:"subject_person_id"`
	SubjectRef      string              `json:"subject_ref"`
	SpanStart       *int64              `json:"span_start"`
	SpanEnd         *int64              `json:"span_end"`
	Excerpt         string              `json:"excerpt"`
	ContentSHA256   string              `json:"content_sha256"`
	SourceVersion   string              `json:"source_version"`
	EventTime       string              `json:"event_time"`
	RecordedTime    string              `json:"recorded_time"`
	IdentityScore   int                 `json:"identity_score"`
}

func evidenceKeyView(input EvidenceInput) canonicalEvidence {
	return canonicalEvidence{
		PersonID: input.PersonID, SourceClass: input.SourceClass,
		Directness: input.Directness, Authority: input.Authority,
		SourceRef: input.SourceRef, SourceURL: input.SourceURL,
		SubjectPersonID: copyInt64Pointer(input.SubjectPersonID), SubjectRef: input.SubjectRef,
		SpanStart: copyInt64Pointer(input.SpanStart), SpanEnd: copyInt64Pointer(input.SpanEnd),
		Excerpt: input.Excerpt, ContentSHA256: input.ContentSHA256,
		SourceVersion: input.SourceVersion,
		EventTime:     portableFactTime(input.EventTime).Format(time.RFC3339Nano),
		RecordedTime:  portableFactTime(input.RecordedTime).Format(time.RFC3339Nano),
		IdentityScore: input.IdentityScore,
	}
}

func evidenceFailure(err error) *ValidationFailure {
	if validation, ok := errors.AsType[*evidenceValidationError](err); ok {
		action := DecisionInvalid
		if validation.reason == ReasonIdentityMismatch {
			action = DecisionIdentityRejected
		}
		return &ValidationFailure{Action: action, Reason: validation.reason, Detail: validation.detail}
	}
	return &ValidationFailure{Action: DecisionInvalid, Reason: ReasonMalformedValue, Detail: err.Error()}
}
