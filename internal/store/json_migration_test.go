package store

import (
	"crypto/sha256"
	"encoding/hex"
	legacyjson "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestAttributeValuePreservesPresentEmptyJSON(t *testing.T) {
	for _, raw := range []string{`{}`, `[]`, `""`, `null`} {
		t.Run(raw, func(t *testing.T) {
			require := require.New(t)
			value := AttributeValue{Type: AttributeValueJSON, JSON: jsontext.Value(raw)}
			encoded, err := json.Marshal(value)
			require.NoError(err)
			assert.JSONEq(t, `{"type":"json","json":`+raw+`}`, string(encoded))
			client := generated.AttributeValue{Type: "json", JSON: jsontext.Value(raw)}
			encoded, err = json.Marshal(client)
			require.NoError(err)
			assert.JSONEq(t, `{"type":"json","json":`+raw+`}`, string(encoded))
		})
	}
}

func TestPersonVCardFingerprintPreservesStoredJSONEncoding(t *testing.T) {
	require := require.New(t)
	snapshot := &PersonVCardSnapshot{}
	legacy, err := legacyjson.Marshal(personVCardFingerprintView(snapshot))
	require.NoError(err)
	digest := sha256.Sum256(legacy)
	got, err := personVCardSnapshotFingerprint(snapshot)
	require.NoError(err)
	assert.Equal(t, hex.EncodeToString(digest[:]), got)
}

func TestPersonVCardSnapshotFingerprintKeepsLegacyAttributeShape(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	description := "A note on this person"
	property := "NOTE"
	snapshot := &PersonVCardSnapshot{Attributes: []PersonVCardAttribute{{
		Definition: AttributeDefinition{
			ID: 7, UniversalID: "person.notes", ObjectType: AttributeObjectPerson, Slug: "notes",
			Label: "Notes", Description: &description, ValueType: AttributeValueText,
			FieldType: AttributeFieldText, Cardinality: AttributeCardinalitySingle,
			IsSearchable: true, IsAudited: true, IsActive: true,
			Stability: AttributeStabilityFixed, VCardProperty: &property,
		},
	}}}
	view := personVCardFingerprintView(snapshot)
	encoded, err := json.Marshal(view.Attributes[0].Definition,
		json.Deterministic(true), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true),
		jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
	require.NoError(err)
	type legacyAttributeDefinition struct {
		ID            int64                `json:"id"`
		UniversalID   string               `json:"universal_id"`
		ObjectType    AttributeObjectType  `json:"object_type"`
		Slug          string               `json:"slug"`
		Label         string               `json:"label"`
		Description   *string              `json:"description,omitzero" nullable:"false"`
		ValueType     AttributeValueType   `json:"value_type"`
		FieldType     AttributeFieldType   `json:"field_type"`
		RecordTarget  *string              `json:"record_target,omitzero" nullable:"false"`
		Cardinality   AttributeCardinality `json:"cardinality"`
		DisplayOrder  int64                `json:"display_order"`
		IsRequired    bool                 `json:"is_required"`
		Ownership     AttributeOwnership   `json:"ownership"`
		UICreatable   bool                 `json:"ui_creatable"`
		UIEditable    bool                 `json:"ui_editable"`
		APIMutable    bool                 `json:"api_mutable"`
		IsSearchable  bool                 `json:"is_searchable"`
		IsSensitive   bool                 `json:"is_sensitive"`
		IsAudited     bool                 `json:"is_audited"`
		IsDeletable   bool                 `json:"is_deletable"`
		HistoryExempt bool                 `json:"history_exempt"`
		DerivedSource *string              `json:"derived_source,omitzero" nullable:"false"`
		Options       *AttributeOptions    `json:"options,omitzero" nullable:"false"`
		VCardProperty *string              `json:"vcard_property,omitzero" nullable:"false"`
		IsActive      bool                 `json:"is_active"`
		Revision      int64                `json:"revision"`
		CreatedAt     time.Time            `json:"created_at"`
		UpdatedAt     time.Time            `json:"updated_at"`
	}
	legacy := legacyAttributeDefinition{
		ID: snapshot.Attributes[0].Definition.ID, UniversalID: snapshot.Attributes[0].Definition.UniversalID,
		ObjectType: snapshot.Attributes[0].Definition.ObjectType, Slug: snapshot.Attributes[0].Definition.Slug,
		ValueType: snapshot.Attributes[0].Definition.ValueType, FieldType: snapshot.Attributes[0].Definition.FieldType,
		Cardinality:  snapshot.Attributes[0].Definition.Cardinality,
		IsSearchable: snapshot.Attributes[0].Definition.IsSearchable, IsAudited: snapshot.Attributes[0].Definition.IsAudited,
		VCardProperty: snapshot.Attributes[0].Definition.VCardProperty, IsActive: snapshot.Attributes[0].Definition.IsActive,
	}
	want, err := json.Marshal(legacy,
		json.Deterministic(true), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true),
		jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
	require.NoError(err)
	assert.Equal(string(want), string(encoded))
	assert.NotContains(string(encoded), `"stability"`)
}

func TestPersonVCardSnapshotFingerprintIgnoresStabilityOnlyEdits(t *testing.T) {
	require := require.New(t)
	snapshot := &PersonVCardSnapshot{Attributes: []PersonVCardAttribute{{
		Definition: AttributeDefinition{ID: 7, UniversalID: "person.notes", ObjectType: AttributeObjectPerson, Slug: "notes", IsActive: true, Stability: AttributeStabilityFixed},
	}}}
	before, err := personVCardSnapshotFingerprint(snapshot)
	require.NoError(err)
	snapshot.Attributes[0].Definition.Stability = AttributeStabilityVolatile
	after, err := personVCardSnapshotFingerprint(snapshot)
	require.NoError(err)
	require.Equal(before, after)
}
