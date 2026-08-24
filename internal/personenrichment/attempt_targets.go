package personenrichment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"go.kenn.io/msgvault/internal/jsonexact"
	"go.kenn.io/msgvault/internal/personfacts"
)

const (
	maxDurableAttemptTargets          = 100
	maxDurableAttemptTargetsJSONBytes = 256 << 10
)

// EncodeDurableAttemptTargets returns the bounded canonical representation
// persisted with an asynchronous provider attempt and an independent copy of
// the exact descriptor set represented by those bytes.
func EncodeDurableAttemptTargets(
	targets []personfacts.TargetDescriptor,
) (string, []personfacts.TargetDescriptor, error) {
	canonical, err := canonicalDurableAttemptTargets(targets)
	if err != nil {
		return "", nil, err
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", nil, fmt.Errorf("encode durable attempt targets: %w", err)
	}
	if len(encoded) > maxDurableAttemptTargetsJSONBytes {
		return "", nil, errors.New("durable attempt targets exceed the size limit")
	}
	return string(encoded), cloneDurableAttemptTargets(canonical), nil
}

// DecodeDurableAttemptTargets accepts only the exact canonical representation
// produced above. This prevents a restart from silently widening or rewriting
// the typed schema that was bound before the provider job started.
func DecodeDurableAttemptTargets(raw string) ([]personfacts.TargetDescriptor, error) {
	if raw == "" || len(raw) > maxDurableAttemptTargetsJSONBytes {
		return nil, errors.New("invalid durable attempt targets size")
	}
	data := []byte(raw)
	var targets []personfacts.TargetDescriptor
	if err := jsonexact.Validate(data, &targets); err != nil {
		return nil, fmt.Errorf("validate durable attempt targets: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&targets); err != nil {
		return nil, fmt.Errorf("decode durable attempt targets: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("durable attempt targets contain trailing JSON")
	}
	encoded, canonical, err := EncodeDurableAttemptTargets(targets)
	if err != nil {
		return nil, err
	}
	if encoded != raw {
		return nil, errors.New("durable attempt targets are not canonical")
	}
	return canonical, nil
}

func canonicalDurableAttemptTargets(
	targets []personfacts.TargetDescriptor,
) ([]personfacts.TargetDescriptor, error) {
	if len(targets) == 0 || len(targets) > maxDurableAttemptTargets {
		return nil, errors.New("durable attempt targets count is out of bounds")
	}
	canonical := cloneDurableAttemptTargets(targets)
	sortTargets(canonical)
	seen := make(map[string]struct{}, len(canonical))
	for i, target := range canonical {
		if err := validateExaTargetDescriptor(target); err != nil {
			return nil, fmt.Errorf("invalid durable attempt target %d: %w", i, err)
		}
		identity := targetIdentity(target)
		if _, duplicate := seen[identity]; duplicate {
			return nil, fmt.Errorf("duplicate durable attempt target %q", target.Key)
		}
		seen[identity] = struct{}{}
	}
	return canonical, nil
}

func cloneDurableAttemptTargets(targets []personfacts.TargetDescriptor) []personfacts.TargetDescriptor {
	cloned := slices.Clone(targets)
	for i := range cloned {
		cloned[i].Choices = slices.Clone(cloned[i].Choices)
		cloned[i].Fields = slices.Clone(cloned[i].Fields)
	}
	return cloned
}
