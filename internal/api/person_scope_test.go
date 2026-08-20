package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
)

func TestWritePersonScopeErrorMapsBindingConflict(t *testing.T) {
	server, _ := newTestServerWithMockStore(t)
	response := httptest.NewRecorder()

	server.writePersonScopeError(response,
		resolver.Reference{Kind: resolver.ReferenceParticipant, ID: 40},
		fmt.Errorf("resolve participant: %w", store.ErrPersonBindingConflict), "file")

	assert.Equal(t, http.StatusConflict, response.Code)
	assert.Contains(t, response.Body.String(), `"error":"person_binding_conflict"`)
}

func TestWritePersonScopeErrorMapsMissingPerson(t *testing.T) {
	server, _ := newTestServerWithMockStore(t)
	response := httptest.NewRecorder()

	server.writePersonScopeError(response,
		resolver.Reference{Kind: resolver.ReferencePerson, ID: 40},
		fmt.Errorf("resolve person: %w", store.ErrPersonNotFound), "visual")

	assert.Equal(t, http.StatusNotFound, response.Code)
	assert.Contains(t, response.Body.String(), `"error":"person_not_found"`)
}

func TestWritePersonScopeErrorReportsEmptyIdentityPopulation(t *testing.T) {
	server, _ := newTestServerWithMockStore(t)
	response := httptest.NewRecorder()

	server.writePersonScopeError(response,
		resolver.Reference{Kind: resolver.ReferencePerson, ID: 40},
		resolver.ErrEmptyPopulation, "file")

	assert.Equal(t, http.StatusUnprocessableEntity, response.Code)
	assert.Contains(t, response.Body.String(), `"error":"person_scope_empty"`)
}

func TestWritePersonScopeErrorPreservesStructuredContextErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "deadline", err: fmt.Errorf("resolve person: %w", context.DeadlineExceeded), code: "query_timeout"},
		{name: "canceled", err: fmt.Errorf("resolve person: %w", context.Canceled), code: "query_canceled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, _ := newTestServerWithMockStore(t)
			response := httptest.NewRecorder()

			server.writePersonScopeError(response,
				resolver.Reference{Kind: resolver.ReferencePerson, ID: 40}, test.err, "visual")

			assert.Equal(t, http.StatusServiceUnavailable, response.Code)
			assert.Contains(t, response.Body.String(), `"error":"`+test.code+`"`)
		})
	}
}
