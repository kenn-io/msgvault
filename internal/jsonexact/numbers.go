package jsonexact

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
)

// PreserveNumbers keeps numbers decoded into any as raw JSON values so IDs,
// prices, and canonical documents do not lose precision through float64.
var PreserveNumbers = json.WithUnmarshalers(json.UnmarshalFromFunc(
	func(decoder *jsontext.Decoder, value *any) error {
		if decoder.PeekKind() != '0' {
			return errors.ErrUnsupported
		}
		raw, err := decoder.ReadValue()
		if err != nil {
			return err
		}
		*value = raw.Clone()
		return nil
	},
))
