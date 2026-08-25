package personfacts

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/jsonexact"
)

// NormalizeClaimValue validates a submitted value against its target and
// returns canonical JSON. Submitted-data defects are durable failures, not Go
// errors; errors are reserved for internal encoding failures.
func NormalizeClaimValue(
	target TargetDescriptor, submitted json.RawMessage,
) (*NormalizedValue, *ValidationFailure, error) {
	malformed := func(detail string) (*NormalizedValue, *ValidationFailure, error) {
		return nil, &ValidationFailure{
			Action: DecisionInvalid, Reason: ReasonMalformedValue, Detail: detail,
		}, nil
	}
	unsupported := func(detail string) (*NormalizedValue, *ValidationFailure, error) {
		return nil, &ValidationFailure{
			Action: DecisionInvalid, Reason: ReasonUnsupportedTarget, Detail: detail,
		}, nil
	}

	var canonical []byte
	var err error
	switch {
	case target.Kind == TargetEmployment && target.ValueType == ValueEmployment:
		canonical, err = normalizeEmploymentValue(submitted)
	case target.Kind == TargetAttribute && supportedGenericValueType(target.ValueType):
		canonical, err = normalizeGenericValue(target.ValueType, submitted)
	default:
		return unsupported(fmt.Sprintf(
			"target %s/%s uses unsupported value type %s", target.Kind, target.Key, target.ValueType))
	}
	if err != nil {
		return malformed(err.Error())
	}
	if target.ValueType == ValueText && target.MaxLength > 0 {
		text, err := canonicalScalarString(canonical)
		if err != nil {
			return malformed(fmt.Sprintf("text value: %v", err))
		}
		if utf8.RuneCountInString(text) > target.MaxLength {
			return malformed(fmt.Sprintf("text value exceeds max_length %d", target.MaxLength))
		}
	}

	if len(target.Choices) > 0 {
		choice, err := canonicalScalarString(canonical)
		if err != nil {
			return malformed(fmt.Sprintf("choice value: %v", err))
		}
		matched := false
		for _, option := range target.Choices {
			if choice == option.Value {
				matched = true
				break
			}
		}
		if !matched {
			return malformed(fmt.Sprintf("value %q is not one of the target choices", choice))
		}
	}

	return &NormalizedValue{
		JSON: append(json.RawMessage(nil), canonical...), Fingerprint: fingerprint(canonical),
	}, nil, nil
}

func supportedGenericValueType(valueType ValueType) bool {
	switch valueType {
	case ValueText, ValueInteger, ValueReal, ValueBoolean, ValueDate, ValueTimestamp:
		return true
	default:
		return false
	}
}

func normalizeGenericValue(valueType ValueType, submitted json.RawMessage) ([]byte, error) {
	switch valueType {
	case ValueText:
		var value string
		if err := decodeJSONValue(submitted, &value); err != nil {
			return nil, fmt.Errorf("decode text: %w", err)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, errors.New("text value must not be blank")
		}
		return json.Marshal(value)
	case ValueInteger:
		var value json.Number
		if err := decodeJSONNumber(submitted, &value); err != nil {
			return nil, fmt.Errorf("decode integer: %w", err)
		}
		integer, err := semanticInteger(value.String())
		if err != nil {
			return nil, err
		}
		return []byte(strconv.FormatInt(integer, 10)), nil
	case ValueReal:
		var value json.Number
		if err := decodeJSONNumber(submitted, &value); err != nil {
			return nil, fmt.Errorf("decode real: %w", err)
		}
		return canonicalNumber(value.String())
	case ValueBoolean:
		var value bool
		if err := decodeJSONValue(submitted, &value); err != nil {
			return nil, fmt.Errorf("decode boolean: %w", err)
		}
		return json.Marshal(value)
	case ValueDate:
		var value string
		if err := decodeJSONValue(submitted, &value); err != nil {
			return nil, fmt.Errorf("decode date: %w", err)
		}
		value = strings.TrimSpace(value)
		parsed, err := time.Parse("2006-01-02", value)
		if err != nil || parsed.Format("2006-01-02") != value {
			return nil, fmt.Errorf("date %q must be a YYYY-MM-DD calendar date", value)
		}
		return json.Marshal(value)
	case ValueTimestamp:
		var value string
		if err := decodeJSONValue(submitted, &value); err != nil {
			return nil, fmt.Errorf("decode timestamp: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("timestamp must be RFC3339 with a timezone: %w", err)
		}
		return json.Marshal(parsed.UTC().Format(time.RFC3339Nano))
	default:
		return nil, fmt.Errorf("unsupported generic value type %s", valueType)
	}
}

func normalizeEmploymentValue(submitted json.RawMessage) ([]byte, error) {
	if err := jsonexact.Validate(submitted, EmploymentValue{}); err != nil {
		return nil, fmt.Errorf("validate employment fields: %w", err)
	}
	var value EmploymentValue
	if err := decodeJSONValue(submitted, &value); err != nil {
		return nil, fmt.Errorf("decode employment: %w", err)
	}
	if value.Organization.ID != nil && *value.Organization.ID <= 0 {
		return nil, errors.New("employment organization id must be positive")
	}
	value.Organization.Name = normalizeHumanText(value.Organization.Name)
	if value.Organization.Name == "" {
		return nil, errors.New("employment organization name is required")
	}
	if value.Organization.Domain != "" {
		domain, err := NormalizeDomain(value.Organization.Domain)
		if err != nil {
			return nil, fmt.Errorf("normalize employment organization domain: %w", err)
		}
		value.Organization.Domain = domain
	}
	value.Title = normalizeHumanText(value.Title)
	value.Role = normalizeHumanText(value.Role)
	value.Department = normalizeHumanText(value.Department)
	value.Location = normalizeHumanText(value.Location)
	if err := validatePartialDate("start_date", value.StartDate); err != nil {
		return nil, err
	}
	if err := validatePartialDate("end_date", value.EndDate); err != nil {
		return nil, err
	}
	if partialDateAfterAtSharedPrecision(value.StartDate, value.EndDate) {
		return nil, errors.New("employment start_date must not be after end_date at their shared precision")
	}
	return json.Marshal(value)
}

func partialDateAfterAtSharedPrecision(start, end *PartialDateValue) bool {
	if start == nil || end == nil || start.Year != end.Year {
		return start != nil && end != nil && start.Year > end.Year
	}
	if start.Month == 0 || end.Month == 0 || start.Month != end.Month {
		return start.Month != 0 && end.Month != 0 && start.Month > end.Month
	}
	return start.Day != 0 && end.Day != 0 && start.Day > end.Day
}

func normalizeHumanText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func validatePartialDate(name string, value *PartialDateValue) error {
	if value == nil {
		return nil
	}
	if value.Year < 1 || value.Year > 9999 {
		return fmt.Errorf("%s year must be between 1 and 9999", name)
	}
	if value.Month == 0 {
		if value.Day != 0 {
			return fmt.Errorf("%s day requires a month", name)
		}
		return nil
	}
	if value.Month < 1 || value.Month > 12 {
		return fmt.Errorf("%s month must be between 1 and 12", name)
	}
	if value.Day == 0 {
		return nil
	}
	if value.Day < 1 || value.Day > 31 {
		return fmt.Errorf("%s day is outside the calendar range", name)
	}
	parsed := time.Date(value.Year, time.Month(value.Month), value.Day, 0, 0, 0, 0, time.UTC)
	if parsed.Year() != value.Year || int(parsed.Month()) != value.Month || parsed.Day() != value.Day {
		return fmt.Errorf("%s is not a valid calendar date", name)
	}
	return nil
}

func decodeJSONValue(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func decodeJSONNumber(data []byte, destination *json.Number) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	number, ok := value.(json.Number)
	if !ok {
		return errors.New("value must be a JSON number")
	}
	*destination = number
	return nil
}

func semanticInteger(raw string) (int64, error) {
	negative := strings.HasPrefix(raw, "-")
	unsigned := strings.TrimPrefix(raw, "-")
	mantissa := unsigned
	exponentText := ""
	if index := strings.IndexAny(unsigned, "eE"); index >= 0 {
		mantissa = unsigned[:index]
		exponentText = unsigned[index+1:]
	}
	_, fraction, hasFraction := strings.Cut(mantissa, ".")
	digits := strings.ReplaceAll(mantissa, ".", "")
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return 0, nil
	}
	coefficient := strings.TrimRight(digits, "0")
	trailingZeroes := len(digits) - len(coefficient)

	var exponent int64
	if exponentText != "" {
		var err error
		exponent, err = strconv.ParseInt(exponentText, 10, 64)
		if err != nil {
			if strings.HasPrefix(exponentText, "-") {
				return 0, errors.New("integer value must have no fractional component")
			}
			return 0, errors.New("integer value is outside int64 range")
		}
	}
	adjustment := int64(trailingZeroes)
	if hasFraction {
		adjustment -= int64(len(fraction))
	}
	if adjustment > 0 && exponent > math.MaxInt64-adjustment {
		return 0, errors.New("integer value is outside int64 range")
	}
	if adjustment < 0 && exponent < math.MinInt64-adjustment {
		return 0, errors.New("integer value must have no fractional component")
	}
	scale := exponent + adjustment
	if scale < 0 {
		return 0, errors.New("integer value must have no fractional component")
	}
	if scale > int64(19-len(coefficient)) {
		return 0, errors.New("integer value is outside int64 range")
	}
	expanded := coefficient + strings.Repeat("0", int(scale))
	if negative {
		expanded = "-" + expanded
	}
	value, err := strconv.ParseInt(expanded, 10, 64)
	if err != nil {
		return 0, errors.New("integer value is outside int64 range")
	}
	return value, nil
}

func canonicalNumber(raw string) ([]byte, error) {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return nil, errors.New("real value must be a finite JSON number")
	}
	if value == 0 {
		value = 0
	}
	return []byte(strconv.FormatFloat(value, 'g', -1, 64)), nil
}

func canonicalScalarString(data []byte) (string, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	switch value := value.(type) {
	case string:
		return value, nil
	case json.Number:
		return value.String(), nil
	case bool:
		return strconv.FormatBool(value), nil
	default:
		return "", errors.New("choice must normalize to a scalar value")
	}
}

func canonicalizeRawJSON(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	normalized, err := normalizeJSONNumbers(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func normalizeJSONNumbers(value any) (any, error) {
	switch value := value.(type) {
	case json.Number:
		canonical, err := canonicalSubmittedJSONNumber(value.String())
		if err != nil {
			return nil, err
		}
		return json.Number(canonical), nil
	case []any:
		for i := range value {
			normalized, err := normalizeJSONNumbers(value[i])
			if err != nil {
				return nil, err
			}
			value[i] = normalized
		}
		return value, nil
	case map[string]any:
		for key := range value {
			normalized, err := normalizeJSONNumbers(value[key])
			if err != nil {
				return nil, err
			}
			value[key] = normalized
		}
		return value, nil
	default:
		return value, nil
	}
}

func canonicalSubmittedJSONNumber(raw string) (string, error) {
	negative := strings.HasPrefix(raw, "-")
	unsigned := strings.TrimPrefix(raw, "-")

	mantissa := unsigned
	exponent := new(big.Int)
	if index := strings.IndexAny(unsigned, "eE"); index >= 0 {
		mantissa = unsigned[:index]
		if _, ok := exponent.SetString(unsigned[index+1:], 10); !ok {
			return "", errors.New("invalid submitted JSON number exponent")
		}
	}

	integer, fraction, hasFraction := strings.Cut(mantissa, ".")
	digits := integer
	if hasFraction {
		digits += fraction
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", nil
	}

	coefficient := strings.TrimRight(digits, "0")
	trailingZeroes := len(digits) - len(coefficient)
	exponent.Sub(exponent, big.NewInt(int64(len(fraction))))
	exponent.Add(exponent, big.NewInt(int64(trailingZeroes)))
	if negative {
		coefficient = "-" + coefficient
	}
	if exponent.Sign() == 0 {
		return coefficient, nil
	}
	return coefficient + "e" + exponent.String(), nil
}

func fingerprint(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}
