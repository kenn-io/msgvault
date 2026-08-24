package personenrichment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const HostClaimMappingVersion = "person-enrichment-claims-v1"

var lowercaseSHA256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type ProgramDescriptor struct {
	HostMappingVersion  string `json:"host_mapping_version"`
	AdapterVersion      string `json:"adapter_version"`
	WireSchemaVersion   string `json:"wire_schema_version"`
	GeneratedSchema     bool   `json:"generated_schema"`
	GeneratedSchemaHash string `json:"generated_schema_hash"`
}

func ProgramFingerprint(descriptor ProgramDescriptor) (string, error) {
	if strings.TrimSpace(descriptor.HostMappingVersion) == "" {
		return "", errors.New("host mapping version is required")
	}
	if strings.TrimSpace(descriptor.AdapterVersion) == "" {
		return "", errors.New("adapter version is required")
	}
	if strings.TrimSpace(descriptor.WireSchemaVersion) == "" {
		return "", errors.New("wire schema version is required")
	}
	if descriptor.GeneratedSchema {
		if !lowercaseSHA256Pattern.MatchString(descriptor.GeneratedSchemaHash) {
			return "", errors.New("generated schema hash must be lowercase 64-hex")
		}
	} else if descriptor.GeneratedSchemaHash != "" {
		return "", errors.New("fixed typed schema must not have a generated schema hash")
	}
	canonical, err := json.Marshal(descriptor)
	if err != nil {
		return "", fmt.Errorf("encode program descriptor: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}
