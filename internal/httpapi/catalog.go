package httpapi

import (
	"net/http"

	"github.com/ottermq/otter_cloud/internal/catalog"
)

type serviceTypeJSON struct {
	Code    catalog.ServiceType `json:"code"`
	Name    string              `json:"name"`
	Flavors []catalog.Flavor    `json:"flavors"`
}

// handleCatalog lists what can be created: each service type with its
// active flavors. A service type with none is left out, so the portal never
// offers something that can't be chosen.
func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	flavors, err := catalog.ActiveFlavors(r.Context(), s.db)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	byType := map[catalog.ServiceType][]catalog.Flavor{}
	for _, f := range flavors {
		byType[f.ServiceType] = append(byType[f.ServiceType], f)
	}
	types := []serviceTypeJSON{}
	for _, t := range catalog.ServiceTypes {
		if fs := byType[t]; len(fs) > 0 {
			types = append(types, serviceTypeJSON{Code: t, Name: t.Name(), Flavors: fs})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"service_types": types})
}
