// Package visual contains provider-independent multimodal attachment models,
// eligibility policy, and deterministic input assembly. Media detection and
// the Voyage transport are delegated to go.kenn.io/docbank/document/media and
// document/voyage; this package keeps archive provenance, orchestration, and
// storage.
package visual

import (
	"go.kenn.io/docbank/document/media"

	"go.kenn.io/msgvault/internal/store"
)

const (
	OriginalMediaInputKey = "original"
	MediaKindImage        = "image"
	MediaKindVideo        = "video"
)

type Owner struct {
	MessageID     int64
	BlobHash      string
	MediaInputKey string
}

type InputPart struct {
	Text  string
	Media *MediaInput
}

type MediaInput struct {
	Kind       string
	Format     media.Format
	MIMEType   string
	BlobHash   string
	Bytes      []byte
	Width      int64
	Height     int64
	DurationMS int64
	Animated   bool
}

type DocumentInput struct {
	Owner          Owner
	Revision       string
	SourceSequence int64
	Parts          []InputPart
}

type QueryInput struct {
	Text  string
	Image *MediaInput
}

type EligibilityReason string

const (
	ReasonRoleUnknown               EligibilityReason = "role_unknown"
	ReasonRoleIneligible            EligibilityReason = "role_ineligible"
	ReasonContentUnavailable        EligibilityReason = "content_unavailable"
	ReasonUnsupportedMediaType      EligibilityReason = "unsupported_media_type"
	ReasonProviderFormatUnsupported EligibilityReason = "provider_format_unsupported"
	ReasonProviderLimit             EligibilityReason = "provider_limit"
	ReasonPixelLimit                EligibilityReason = "pixel_limit"
	ReasonMalformedMedia            EligibilityReason = "malformed_media"
	ReasonEmptyOrExcludedAsset      EligibilityReason = "empty_or_excluded_asset"
)

type Eligibility struct {
	Eligible bool
	Reason   EligibilityReason
	Media    *MediaInput
}

type Occurrence struct {
	MessageID    int64
	BlobHash     string
	DeclaredMIME string
	Role         store.AttachmentRole
	RoleSource   store.AttachmentRoleSource
}
