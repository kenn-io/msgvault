package peoplebrowser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestNewFieldDefinitionInputMapsEditableKinds(t *testing.T) {
	tests := []struct {
		name      string
		kind      FieldKind
		valueType store.AttributeValueType
		fieldType store.AttributeFieldType
	}{
		{name: "Text", kind: FieldKindText, valueType: store.AttributeValueText, fieldType: store.AttributeFieldText},
		{name: "Long text", kind: FieldKindLongText, valueType: store.AttributeValueText, fieldType: store.AttributeFieldTextarea},
		{name: "Number", kind: FieldKindNumber, valueType: store.AttributeValueReal, fieldType: store.AttributeFieldText},
		{name: "Checkbox", kind: FieldKindCheckbox, valueType: store.AttributeValueBoolean, fieldType: store.AttributeFieldCheckbox},
		{name: "Date", kind: FieldKindDate, valueType: store.AttributeValueDate, fieldType: store.AttributeFieldDate},
		{name: "Date/time", kind: FieldKindDateTime, valueType: store.AttributeValueTimestamp, fieldType: store.AttributeFieldTimestamp},
		{name: "URL", kind: FieldKindURL, valueType: store.AttributeValueText, fieldType: store.AttributeFieldURL},
		{name: "Email", kind: FieldKindEmail, valueType: store.AttributeValueText, fieldType: store.AttributeFieldEmail},
		{name: "Phone", kind: FieldKindPhone, valueType: store.AttributeValueText, fieldType: store.AttributeFieldPhone},
		{name: "JSON", kind: FieldKindJSON, valueType: store.AttributeValueJSON, fieldType: store.AttributeFieldJSON},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			got, err := (NewField{
				Label:       "Favorite " + tt.name,
				Kind:        tt.kind,
				Cardinality: store.AttributeCardinalityMulti,
			}).DefinitionInput()
			require.NoError(t, err)
			assert.Equal("Favorite "+tt.name, got.Label)
			assert.Equal(tt.valueType, got.ValueType)
			assert.Equal(tt.fieldType, got.FieldType)
			assert.Equal(store.AttributeCardinalityMulti, got.Cardinality)
		})
	}
}

func TestEditableFieldKindsIncludesEverySupportedKind(t *testing.T) {
	assert.Equal(t, []FieldKind{
		FieldKindText,
		FieldKindLongText,
		FieldKindNumber,
		FieldKindCheckbox,
		FieldKindDate,
		FieldKindDateTime,
		FieldKindURL,
		FieldKindEmail,
		FieldKindPhone,
		FieldKindJSON,
	}, EditableFieldKinds)
}

func TestNewFieldDefinitionInputRejectsUnknownKind(t *testing.T) {
	_, err := (NewField{Kind: FieldKind("select")}).DefinitionInput()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "select")
}
