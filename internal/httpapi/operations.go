package httpapi

import (
	"errors"
	"net/http"

	"github.com/ottermq/otter_cloud/internal/instances"
)

// Operations are read-only for customers (portals.md §3): the platform
// starts them, and the portal polls one while it runs.

func (s *Server) handleOperation(w http.ResponseWriter, r *http.Request) {
	op, err := instances.OperationByID(r.Context(), s.db, accountOf(r), r.PathValue("id"))
	if errors.Is(err, instances.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such operation")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func (s *Server) handleOperations(w http.ResponseWriter, r *http.Request) {
	ops, err := instances.AccountOperations(r.Context(), s.db, accountOf(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeOperations(w, ops)
}

func (s *Server) handleInstanceOperations(w http.ResponseWriter, r *http.Request) {
	ops, err := instances.InstanceOperations(r.Context(), s.db, accountOf(r), r.PathValue("id"))
	if errors.Is(err, instances.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such database")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeOperations(w, ops)
}

func writeOperations(w http.ResponseWriter, ops []instances.OperationView) {
	if ops == nil {
		ops = []instances.OperationView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"operations": ops})
}

// accountOf is the account of the request's session. Every customer query
// is scoped by it.
func accountOf(r *http.Request) string { return sessionFrom(r.Context()).User.AccountID }
