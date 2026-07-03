package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/collectionops"
)

type cliCollectionCreateInput struct {
	Body collectionops.CreateRequest
}

type cliCollectionSourcesInput struct {
	Name string `path:"name" doc:"Collection name"`
	Body collectionops.SourcesRequest
}

type cliCollectionDeleteInput struct {
	Name string `path:"name" doc:"Collection name"`
}

type cliCollectionMutationOutput struct {
	Body collectionops.MutationResult
}

func (s *Server) registerCLICollectionHumaRoutes(api huma.API) {
	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID:      "createCLICollection",
		Method:           http.MethodPost,
		Path:             "/cli/collections",
		Tags:             []string{cliRouteTag},
		Summary:          "Create a collection for CLI use",
		SkipValidateBody: true,
		Errors:           []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(_ context.Context, input *cliCollectionCreateInput) (*cliCollectionMutationOutput, error) {
		result, err := s.createCLICollection(input.Body)
		if err != nil {
			return nil, err
		}
		return &cliCollectionMutationOutput{Body: result}, nil
	})

	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID:      "addCLICollectionSources",
		Method:           http.MethodPatch,
		Path:             "/cli/collections/{name}/sources",
		Tags:             []string{cliRouteTag},
		Summary:          "Add accounts to a CLI collection",
		SkipValidateBody: true,
		Errors:           []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(_ context.Context, input *cliCollectionSourcesInput) (*cliCollectionMutationOutput, error) {
		result, err := s.addCLICollectionSources(input.Name, input.Body)
		if err != nil {
			return nil, err
		}
		return &cliCollectionMutationOutput{Body: result}, nil
	})

	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID:      "removeCLICollectionSources",
		Method:           http.MethodDelete,
		Path:             "/cli/collections/{name}/sources",
		Tags:             []string{cliRouteTag},
		Summary:          "Remove accounts from a CLI collection",
		SkipValidateBody: true,
		Errors:           []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(_ context.Context, input *cliCollectionSourcesInput) (*cliCollectionMutationOutput, error) {
		result, err := s.removeCLICollectionSources(input.Name, input.Body)
		if err != nil {
			return nil, err
		}
		return &cliCollectionMutationOutput{Body: result}, nil
	})

	huma.Register(api, withAPIKeySecurity(huma.Operation{
		OperationID: "deleteCLICollection",
		Method:      http.MethodDelete,
		Path:        "/cli/collections/{name}",
		Tags:        []string{cliRouteTag},
		Summary:     "Delete a CLI collection",
		Errors:      []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable},
	}), func(_ context.Context, input *cliCollectionDeleteInput) (*cliCollectionMutationOutput, error) {
		result, err := s.deleteCLICollection(input.Name)
		if err != nil {
			return nil, err
		}
		return &cliCollectionMutationOutput{Body: result}, nil
	})
}
