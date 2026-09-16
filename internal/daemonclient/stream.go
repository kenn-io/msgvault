package daemonclient

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
)

func decodeCLIStream[T any](
	body io.Reader,
	operation string,
	handle func(T) (complete bool, err error),
) error {
	complete := false
	dec := jsontext.NewDecoder(body)
	for {
		var event T
		err := json.UnmarshalDecode(dec, &event)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("decode CLI %s stream: %w", operation, err)
		}
		eventComplete, err := handle(event)
		if err != nil {
			return err
		}
		if eventComplete {
			complete = true
		}
	}
	if !complete {
		return fmt.Errorf("%s stream ended without completion", operation)
	}
	return nil
}
