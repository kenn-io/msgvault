package vcard

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResourceOwnershipMatchesDuplicateOccurrences(t *testing.T) {
	const prefix = "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:person\r\nFN:Person\r\n"
	const notes = "NOTE:Same\r\nNOTE:Same\r\n"
	const suffix = "END:VCARD\r\n"
	prepared, err := ParseResourceEnvelope([]byte(prefix + notes + suffix))
	require.NoError(t, err)
	for _, occurrence := range prepared.PropertyTree {
		if occurrence.Property.Name == "NOTE" {
			prepared.NativeMappings = append(prepared.NativeMappings, NativeMapping{Identity: occurrence.Identity, Table: "person_attribute_values", RowID: int64(occurrence.Identity.Ordinal), Field: "value", Kind: HandlingDerived})
		}
	}
	// Mapping storage order must not change which duplicate owns each row.
	prepared.NativeMappings[0], prepared.NativeMappings[1] = prepared.NativeMappings[1], prepared.NativeMappings[0]
	for _, tc := range []struct {
		name     string
		body     string
		mismatch bool
	}{
		{"identical", prefix + notes + suffix, false},
		{"server property", prefix + "PRODID:Server\r\n" + notes + suffix, false},
		{"extra duplicate", prefix + notes + "NOTE:Same\r\n" + suffix, true},
		{"missing duplicate", prefix + "NOTE:Same\r\n" + suffix, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			canonical, err := ParseResourceEnvelope([]byte(tc.body))
			require.NoError(err)
			rebound, err := RebindResourceOwnership(prepared, canonical, true)
			if tc.mismatch {
				require.ErrorIs(err, ErrResourceOwnershipMismatch)
				return
			}
			require.NoError(err)
			require.Len(rebound.NativeMappings, 2)
			shift := 0
			if strings.Contains(tc.body, "PRODID:") {
				shift = 1
			}
			for i, mapping := range rebound.NativeMappings {
				assert.Equal(prepared.NativeMappings[i].RowID, mapping.RowID)
				assert.Equal(prepared.NativeMappings[i].Identity.Ordinal+shift, mapping.Identity.Ordinal)
			}
			for _, residue := range rebound.Residue {
				assert.NotEqual("NOTE", residue.Property.Name)
			}
		})
	}
}

func TestResourceOwnershipPreservesForeignMappingAndLeavesChangedValuesAsResidue(t *testing.T) {
	prepared, err := ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:person\r\nFN:Person\r\nNOTE:Original\r\nEND:VCARD\r\n"))
	require.NoError(t, err)
	prepared.SourceRef = "carddav:1"
	for _, occurrence := range prepared.PropertyTree {
		if occurrence.Property.Name == "NOTE" {
			prepared.NativeMappings = []NativeMapping{{Identity: occurrence.Identity, SourceRef: "retained-source", Table: "person_attribute_values", RowID: 7, Field: "value", Kind: HandlingDerived}}
		}
	}
	for _, text := range []string{"Original", "Remote change"} {
		t.Run(text, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			incoming, err := ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:person\r\nFN:Person\r\nNOTE:" + text + "\r\nEND:VCARD\r\n"))
			require.NoError(err)
			incoming.SourceRef = prepared.SourceRef
			rebound, err := RebindResourceOwnership(prepared, incoming, false)
			require.NoError(err)
			assert.Equal(incoming.StoredBody, rebound.StoredBody)
			if text == "Original" {
				require.Len(rebound.NativeMappings, 1)
				assert.Equal("retained-source", rebound.NativeMappings[0].SourceRef)
			} else {
				assert.Empty(rebound.NativeMappings)
				noteResidue := false
				for _, occurrence := range rebound.Residue {
					if occurrence.Property.Name == "NOTE" {
						noteResidue = true
					}
				}
				assert.True(noteResidue)
			}
		})
	}
}
