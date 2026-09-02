package peoplesweep_test

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func TestDefaultPersonInputBudgetCoversMaximumStructuredWireRequest(t *testing.T) {
	requirements := require.New(t)
	config := validConfig()
	profile, err := config.Profile()
	requirements.NoError(err)
	request := peoplesweep.StructuredRequest{
		ProgramID:      peoplesweep.ExtractionProgramID,
		ProgramVersion: peoplesweep.ExtractionProgramVersion,
		Sources: []peoplesweep.SourceDescriptor{{
			Class: peoplesweep.SourceConversationText, ObservedOn: "2026-08-24",
		}},
		InputText:       strings.Repeat("x", 128<<10),
		SchemaName:      peoplesweep.ExtractionSchemaName,
		JSONSchema:      peoplesweep.ExtractionJSONSchema(),
		MaxOutputTokens: 4096,
	}
	prepared, err := peoplesweep.NewOpenAIChatDriver(http.DefaultClient).Prepare(profile, request)
	requirements.NoError(err)
	usage, err := peoplesweep.EstimateWireTokenReservation(prepared.WireRequest(), request.MaxOutputTokens)
	requirements.NoError(err)

	assert.LessOrEqual(t, usage.InputTokens, config.Budgets.MaxInputTokensPerPerson)
}

func TestEstimateSweepWireUsageChargesAtLeastOneTokenPerByte(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	wire, err := json.Marshal(map[string]any{
		"system": "fixed person sweep instructions",
		"response_format": map[string]any{"schema": map[string]any{
			"type": "object", "required": []string{"claims"},
		}},
		"input": map[string]any{
			"packet": map[string]any{"person_id": 42, "excerpt": "unicode ☃"},
		},
		"provider_wrapper":  map[string]any{"model": "synthetic-model"},
		"max_output_tokens": 4096,
	})
	requirements.NoError(err)

	usage, err := peoplesweep.EstimateWireTokenReservation(wire, 4096)
	requirements.NoError(err)
	checks.Equal(int64(len(wire)), usage.InputTokens)
	checks.Equal(int64(4096), usage.OutputTokens)

	_, err = peoplesweep.EstimateWireTokenReservation(nil, 4096)
	requirements.Error(err)
	_, err = peoplesweep.EstimateWireTokenReservation(wire, 0)
	requirements.Error(err)
}

func TestEstimateSweepCostRejectsOverflow(t *testing.T) {
	requirements := require.New(t)
	budget := peoplesweep.BudgetConfig{
		InputCostMicroUSDPerMillionTokens:  2_000_000,
		OutputCostMicroUSDPerMillionTokens: 3_000_000,
	}
	cost, err := peoplesweep.EstimateCostMicroUSD(peoplesweep.TokenUsage{
		InputTokens: 1_000_001, OutputTokens: 2_000_001,
	}, budget)
	requirements.NoError(err)
	assert.Equal(t, int64(8_000_005), cost)

	_, err = peoplesweep.EstimateCostMicroUSD(peoplesweep.TokenUsage{
		InputTokens: math.MaxInt64,
	}, peoplesweep.BudgetConfig{InputCostMicroUSDPerMillionTokens: math.MaxInt64})
	requirements.ErrorIs(err, peoplesweep.ErrBudgetOverflow)

	_, err = peoplesweep.EstimateCostMicroUSD(peoplesweep.TokenUsage{InputTokens: -1}, budget)
	requirements.Error(err)
}
