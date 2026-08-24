package personenrichment_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
)

func TestProgramFingerprintStableAndSensitive(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	descriptor := personenrichment.ProgramDescriptor{
		HostMappingVersion:  personenrichment.HostClaimMappingVersion,
		AdapterVersion:      "exa-adapter-v1",
		WireSchemaVersion:   "exa-response-v1",
		GeneratedSchema:     true,
		GeneratedSchemaHash: strings.Repeat("a", 64),
	}
	baseline, err := personenrichment.ProgramFingerprint(descriptor)
	requirements.NoError(err)
	again, err := personenrichment.ProgramFingerprint(descriptor)
	requirements.NoError(err)
	checks.Equal(baseline, again)
	checks.Regexp(`^[a-f0-9]{64}$`, baseline)

	mutations := []struct {
		name   string
		mutate func(*personenrichment.ProgramDescriptor)
	}{
		{"host mapping", func(d *personenrichment.ProgramDescriptor) { d.HostMappingVersion = "person-enrichment-claims-v2" }},
		{"adapter", func(d *personenrichment.ProgramDescriptor) { d.AdapterVersion = "exa-adapter-v2" }},
		{"wire schema", func(d *personenrichment.ProgramDescriptor) { d.WireSchemaVersion = "exa-response-v2" }},
		{"generated schema hash", func(d *personenrichment.ProgramDescriptor) { d.GeneratedSchemaHash = strings.Repeat("b", 64) }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := descriptor
			mutation.mutate(&changed)
			fingerprint, fingerprintErr := personenrichment.ProgramFingerprint(changed)
			require.NoError(t, fingerprintErr)
			assert.NotEqual(t, baseline, fingerprint)
		})
	}

	fixed := descriptor
	fixed.GeneratedSchema = false
	fixed.GeneratedSchemaHash = ""
	fixedFingerprint, err := personenrichment.ProgramFingerprint(fixed)
	requirements.NoError(err)
	checks.NotEqual(baseline, fixedFingerprint)

	invalid := []personenrichment.ProgramDescriptor{
		{AdapterVersion: "a", WireSchemaVersion: "w"},
		{HostMappingVersion: personenrichment.HostClaimMappingVersion, WireSchemaVersion: "w"},
		{HostMappingVersion: personenrichment.HostClaimMappingVersion, AdapterVersion: "a"},
		{HostMappingVersion: personenrichment.HostClaimMappingVersion, AdapterVersion: "a", WireSchemaVersion: "w", GeneratedSchema: true},
		{HostMappingVersion: personenrichment.HostClaimMappingVersion, AdapterVersion: "a", WireSchemaVersion: "w", GeneratedSchema: true, GeneratedSchemaHash: strings.Repeat("A", 64)},
		{HostMappingVersion: personenrichment.HostClaimMappingVersion, AdapterVersion: "a", WireSchemaVersion: "w", GeneratedSchema: true, GeneratedSchemaHash: "not-a-hash"},
		{HostMappingVersion: personenrichment.HostClaimMappingVersion, AdapterVersion: "a", WireSchemaVersion: "w", GeneratedSchemaHash: strings.Repeat("a", 64)},
	}
	for index, value := range invalid {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			_, fingerprintErr := personenrichment.ProgramFingerprint(value)
			require.Error(t, fingerprintErr)
		})
	}
}
