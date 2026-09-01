package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/opserr"
)

type cliDeleteDedupedPlanInput struct {
	Body cliDeleteDedupedPlanRequest
}

type cliDeduplicatePlanInput struct {
	Body CLIDeduplicatePlanRequest
}

type cliDeleteDedupedExecuteInput struct {
	Body cliDeleteDedupedExecuteRequest
}

type cliDeleteDedupedPlanOutput struct {
	Body cliDeleteDedupedPlanResponse
}

type cliDeduplicatePlanOutput struct {
	Body CLIDeduplicatePlanResponse
}

type cliDeleteDedupedExecuteOutput struct {
	Body cliDeleteDedupedExecuteResponse
}

func (s *Server) registerCLIDedupHumaRoutes(api huma.API) {
	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID:      "planCLIDeduplicate",
		Method:           http.MethodPost,
		Path:             "/cli/deduplicate/plan",
		Tags:             []string{cliRouteTag},
		Summary:          "Plan interactive deduplication for CLI use",
		SkipValidateBody: true,
		Errors:           []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(ctx context.Context, input *cliDeduplicatePlanInput) (*cliDeduplicatePlanOutput, error) {
		if input.Body.PlanProtocol != apiprotocol.DeduplicatePlanProtocol {
			return nil, s.operationError(
				opserr.Invalid(errors.New("deduplicate planning requires a compatible CLI; upgrade msgvault and retry")),
				deduplicatePlanOperationErrorPolicy,
				"Deduplicate planning failed",
			)
		}
		planner, ok := s.store.(CLIDeduplicatePlanner)
		if !ok {
			return nil, cliStoreUnavailableError()
		}
		result, err := planner.PlanCLIDeduplicate(ctx, input.Body)
		if err != nil {
			return nil, s.operationError(
				err,
				deduplicatePlanOperationErrorPolicy,
				"Deduplicate planning failed",
			)
		}
		return &cliDeduplicatePlanOutput{Body: result}, nil
	})

	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID:      "planCLIDeleteDeduped",
		Method:           http.MethodPost,
		Path:             "/cli/delete-deduped/plan",
		Tags:             []string{cliRouteTag},
		Summary:          "Plan deletion of dedup-hidden messages for CLI use",
		SkipValidateBody: true,
		Errors:           []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(ctx context.Context, input *cliDeleteDedupedPlanInput) (*cliDeleteDedupedPlanOutput, error) {
		result, err := s.planCLIDeleteDeduped(ctx, input.Body)
		if err != nil {
			return nil, err
		}
		return &cliDeleteDedupedPlanOutput{Body: result}, nil
	})

	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID:      "executeCLIDeleteDeduped",
		Method:           http.MethodPost,
		Path:             "/cli/delete-deduped",
		Tags:             []string{cliRouteTag},
		Summary:          "Delete dedup-hidden messages for CLI use",
		SkipValidateBody: true,
		Errors:           []int{http.StatusBadRequest, http.StatusConflict, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(ctx context.Context, input *cliDeleteDedupedExecuteInput) (*cliDeleteDedupedExecuteOutput, error) {
		result, err := s.executeCLIDeleteDeduped(ctx, input.Body)
		if err != nil {
			return nil, err
		}
		return &cliDeleteDedupedExecuteOutput{Body: result}, nil
	})
}
