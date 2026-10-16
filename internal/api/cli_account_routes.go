package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/accountops"
	"go.kenn.io/msgvault/internal/store"
)

type cliAccountUpdateInput struct {
	Body accountops.UpdateRequest
}

type cliAccountUpdateOutput struct {
	Body accountops.UpdateResult
}

type cliAccountMergeInput struct{ Body accountops.MergeRequest }
type cliAccountMergeOutput struct{ Body store.SourceMergeResult }

func (s *Server) registerCLIAccountHumaRoutes(api huma.API) {
	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID: "mergeCLIAccounts", Method: http.MethodPost, Path: "/cli/account/merge",
		Tags: []string{cliRouteTag}, Summary: "Merge one archive source into another", SkipValidateBody: true,
		Errors: []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(ctx context.Context, input *cliAccountMergeInput) (*cliAccountMergeOutput, error) {
		st, ok := s.store.(accountops.MergeStore)
		if !ok {
			return nil, huma.Error503ServiceUnavailable("Source maintenance is unavailable")
		}
		result, err := accountops.MergeAccounts(ctx, st, input.Body)
		if err != nil {
			return nil, s.operationError(err, accountOperationErrorPolicy, "Failed to merge accounts")
		}
		// A committed merge moves data read by the analytics cache and bumps
		// account-identity revision. Dry runs and journaled retries do neither.
		if !result.DryRun && !result.AlreadyMerged {
			s.scheduleAccountIdentityCacheRebuild(ctx)
		}
		return &cliAccountMergeOutput{Body: result}, nil
	})
	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID:      "updateCLIAccount",
		Method:           http.MethodPost,
		Path:             "/cli/account",
		Tags:             []string{cliRouteTag},
		Summary:          "Update an account for CLI use",
		SkipValidateBody: true,
		Errors:           []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(ctx context.Context, input *cliAccountUpdateInput) (*cliAccountUpdateOutput, error) {
		result, err := s.updateCLIAccount(ctx, input.Body)
		if err != nil {
			return nil, err
		}
		return &cliAccountUpdateOutput{Body: result}, nil
	})
}
