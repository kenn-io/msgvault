package api

import (
	"context"
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/identityops"
)

type cliIdentityListInput struct {
	Account     string                   `query:"account" doc:"Restrict to a single account"`
	Collection  string                   `query:"collection" doc:"Restrict to all member accounts of one collection"`
	SourceID    cliIdentitySourceIDParam `query:"source_id" doc:"Restrict to one source by numeric ID"`
	PrimaryOnly bool                     `query:"primary_only" doc:"For account scope, return only the primary source instead of related sources"`
}

type cliIdentitySourceIDParam struct {
	Value int64
	IsSet bool
}

func (p *cliIdentitySourceIDParam) Schema(r huma.Registry) *huma.Schema {
	return huma.SchemaFromType(r, reflect.TypeFor[int64]())
}

func (p *cliIdentitySourceIDParam) Receiver() reflect.Value {
	return reflect.ValueOf(p).Elem().Field(0)
}

func (p *cliIdentitySourceIDParam) OnParamSet(isSet bool, _ any) {
	p.IsSet = isSet
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

type cliIdentityImportInput struct {
	Body identityops.ImportRequest
}

type cliIdentityImportOutput struct {
	Body identityops.ImportResult
}

func (s *Server) registerCLIIdentityHumaRoutes(api huma.API) {
	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID: "listCLIIdentities",
		Method:      http.MethodGet,
		Path:        "/cli/identities",
		Tags:        []string{cliRouteTag},
		Summary:     "List confirmed account identities",
		Errors:      []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(ctx context.Context, input *cliIdentityListInput) (*cliIdentitiesOutput, error) {
		resp, err := s.getCLIIdentities(
			ctx, input.Account, input.Collection, input.SourceID.Value, input.SourceID.IsSet, input.PrimaryOnly,
		)
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

	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID:      "importCLIIdentities",
		Method:           http.MethodPost,
		Path:             "/cli/identities/import",
		Tags:             []string{cliRouteTag},
		Summary:          "Preview or apply parsed source-scoped identities",
		SkipValidateBody: true,
		Errors:           []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(ctx context.Context, input *cliIdentityImportInput) (*cliIdentityImportOutput, error) {
		result, err := s.importCLIIdentities(ctx, input.Body)
		if err != nil {
			return nil, err
		}
		return &cliIdentityImportOutput{Body: result}, nil
	})
}

func (s *Server) registerCLIIdentityDiscoveryRoute(api huma.API) {
	registerAPIV1RawHumaNDJSONRouteWithRequest[identityops.DiscoverRequest, identityops.DiscoverEvent](
		api,
		"discoverCLIIdentities",
		http.MethodPost,
		"/cli/identities/discover",
		"Discover source-scoped account identities",
		s.handleCLIIdentityDiscover,
	)
}
