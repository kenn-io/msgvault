package peoplesweep

import (
	"errors"
	"math"
)

const tokensPerMillion = int64(1_000_000)

func EstimateWireTokenReservation(wireRequest []byte, maxOutputTokens int) (TokenUsage, error) {
	if len(wireRequest) == 0 {
		return TokenUsage{}, errors.New("estimate sweep wire usage: request is empty")
	}
	if maxOutputTokens <= 0 {
		return TokenUsage{}, errors.New("estimate sweep wire usage: maximum output must be positive")
	}
	if uint64(len(wireRequest)) > uint64(math.MaxInt64) {
		return TokenUsage{}, ErrBudgetOverflow
	}
	return TokenUsage{InputTokens: int64(len(wireRequest)), OutputTokens: int64(maxOutputTokens)}, nil
}

func EstimateCostMicroUSD(usage TokenUsage, budget BudgetConfig) (int64, error) {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 ||
		budget.InputCostMicroUSDPerMillionTokens < 0 ||
		budget.OutputCostMicroUSDPerMillionTokens < 0 {
		return 0, errors.New("estimate sweep cost: usage and prices must not be negative")
	}
	input, err := ceilingProductPerMillion(usage.InputTokens,
		budget.InputCostMicroUSDPerMillionTokens)
	if err != nil {
		return 0, err
	}
	output, err := ceilingProductPerMillion(usage.OutputTokens,
		budget.OutputCostMicroUSDPerMillionTokens)
	if err != nil {
		return 0, err
	}
	if input > math.MaxInt64-output {
		return 0, ErrBudgetOverflow
	}
	return input + output, nil
}

func ceilingProductPerMillion(tokens, price int64) (int64, error) {
	if tokens == 0 || price == 0 {
		return 0, nil
	}
	if tokens > math.MaxInt64/price {
		return 0, ErrBudgetOverflow
	}
	product := tokens * price
	quotient := product / tokensPerMillion
	if product%tokensPerMillion != 0 {
		if quotient == math.MaxInt64 {
			return 0, ErrBudgetOverflow
		}
		quotient++
	}
	return quotient, nil
}

func ValidateBudgetConfig(budget BudgetConfig) error {
	if budget.MaxRequestsPerPerson <= 0 || budget.MaxInputTokensPerPerson <= 0 ||
		budget.MaxOutputTokensPerPerson <= 0 || budget.MaxRequestsPerRun <= 0 ||
		budget.MaxInputTokensPerRun <= 0 || budget.MaxOutputTokensPerRun <= 0 ||
		budget.MaxRequestsPerDay <= 0 || budget.MaxInputTokensPerDay <= 0 ||
		budget.MaxOutputTokensPerDay <= 0 {
		return errors.New("person sweep budget caps must be positive")
	}
	if budget.MaxRequestsPerPerson > math.MaxInt32 ||
		budget.MaxRequestsPerRun > math.MaxInt32 || budget.MaxRequestsPerDay > math.MaxInt32 {
		return errors.New("person sweep request caps exceed durable accounting range")
	}
	if budget.MaxEstimatedCostMicroUSDPerRun < 0 ||
		budget.MaxEstimatedCostMicroUSDPerDay < 0 ||
		budget.InputCostMicroUSDPerMillionTokens < 0 ||
		budget.OutputCostMicroUSDPerMillionTokens < 0 {
		return errors.New("person sweep budget costs must not be negative")
	}
	if (budget.MaxEstimatedCostMicroUSDPerRun > 0 ||
		budget.MaxEstimatedCostMicroUSDPerDay > 0) &&
		(budget.InputCostMicroUSDPerMillionTokens <= 0 ||
			budget.OutputCostMicroUSDPerMillionTokens <= 0) {
		return errors.New("person sweep budget prices are required for cost caps")
	}
	return nil
}

// IsSafeProviderMetadata reports whether a provider-owned identifier is safe
// to retain in redacted durable sweep history. Empty values are allowed when
// the provider did not return an identifier.
func IsSafeProviderMetadata(value string) bool {
	return value == "" || safeProviderMetadata(value)
}
