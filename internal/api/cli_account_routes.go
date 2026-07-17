package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/accountops"
)

type cliAccountUpdateInput struct {
	Body accountops.UpdateRequest
}

type cliAccountUpdateOutput struct {
	Body accountops.UpdateResult
}

func (s *Server) registerCLIAccountHumaRoutes(api huma.API) {
	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID:      "updateCLIAccount",
		Method:           http.MethodPost,
		Path:             "/cli/account",
		Tags:             []string{cliRouteTag},
		Summary:          "Update an account for CLI use",
		SkipValidateBody: true,
		Errors:           []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(_ context.Context, input *cliAccountUpdateInput) (*cliAccountUpdateOutput, error) {
		result, err := s.updateCLIAccount(input.Body)
		if err != nil {
			return nil, err
		}
		return &cliAccountUpdateOutput{Body: result}, nil
	})
}
