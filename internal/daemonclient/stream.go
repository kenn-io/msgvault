package daemonclient

import (
	"encoding/json"
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
	dec := json.NewDecoder(body)
	for {
		var event T
		err := dec.Decode(&event)
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
