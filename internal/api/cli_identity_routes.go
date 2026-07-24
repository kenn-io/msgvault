package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/identityops"
)

type cliIdentityListInput struct {
	Account     string `query:"account" doc:"Restrict to a single account"`
	Collection  string `query:"collection" doc:"Restrict to all member accounts of one collection"`
	PrimaryOnly bool   `query:"primary_only" doc:"For account scope, return only the primary source instead of related sources"`
}

type cliIdentitiesOutput struct {
	Body cliIdentitiesResponse
}

type cliIdentityAddInput struct {
	Body identityops.AddRequest
}

type cliIdentityAddOutput struct {
	Body identityops.AddResult
}

type cliIdentityRemoveInput struct {
	Body identityops.RemoveRequest
}

type cliIdentityRemoveOutput struct {
	Body identityops.RemoveResult
}

func (s *Server) registerCLIIdentityHumaRoutes(api huma.API) {
	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID: "listCLIIdentities",
		Method:      http.MethodGet,
		Path:        "/cli/identities",
		Tags:        []string{cliRouteTag},
		Summary:     "List confirmed account identities",
		Errors:      []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(_ context.Context, input *cliIdentityListInput) (*cliIdentitiesOutput, error) {
		resp, err := s.getCLIIdentities(input.Account, input.Collection, input.PrimaryOnly)
		if err != nil {
			return nil, err
		}
		return &cliIdentitiesOutput{Body: resp}, nil
	})

	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID:      "addCLIIdentity",
		Method:           http.MethodPost,
		Path:             "/cli/identities",
		Tags:             []string{cliRouteTag},
		Summary:          "Add a confirmed identifier to an account identity",
		SkipValidateBody: true,
		Errors:           []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(ctx context.Context, input *cliIdentityAddInput) (*cliIdentityAddOutput, error) {
		result, err := s.addCLIIdentity(ctx, input.Body)
		if err != nil {
			return nil, err
		}
		return &cliIdentityAddOutput{Body: result}, nil
	})

	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID:      "removeCLIIdentity",
		Method:           http.MethodDelete,
		Path:             "/cli/identities",
		Tags:             []string{cliRouteTag},
		Summary:          "Remove a confirmed identifier from an account identity",
		SkipValidateBody: true,
		Errors:           []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(ctx context.Context, input *cliIdentityRemoveInput) (*cliIdentityRemoveOutput, error) {
		result, err := s.removeCLIIdentity(ctx, input.Body)
		if err != nil {
			return nil, err
		}
		return &cliIdentityRemoveOutput{Body: result}, nil
	})
}
