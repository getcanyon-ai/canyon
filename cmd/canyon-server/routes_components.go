package main

import (
	"context"
	"net/http"
)

func routesComponents(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /component-types", a.componentTypes)
}

// componentTypes answers the snapshot's component_menu as rows, built fresh per request; a read,
// so no audit row. loadSnapshot already refused any menu name the table cannot describe.
func (a *app) componentTypes(w http.ResponseWriter, r *http.Request) {
	rows, err := componentsOf(a.snap.ComponentMenu)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	rows, err = a.provisioned(r.Context(), rows)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"component_types": rows})
}

// provisioned marks a type available on a host with a driver for it in preview, where a deploy first needs one,
// and every type under run_by: org, whose platform provides it (#2432).
func (a *app) provisioned(ctx context.Context, rows []componentType) ([]componentType, error) {
	if len(rows) == 0 || a.pipe == nil { // nothing to mark, or no pipeline to run anything: all stay planned
		return rows, nil
	}
	org, err := runByOrg(ctx, a.pool)
	if err != nil {
		return nil, err
	}
	return a.offered(rows, org), nil
}

// offered marks the rows as provisioned says, run_by read: org, or not.
func (a *app) offered(rows []componentType, org bool) []componentType {
	for i := range rows {
		if org {
			rows[i].Status = "available"
		} else if a.pipe != nil {
			if _, err := a.pipe.Drivers.Select(rows[i].Type, "preview"); err == nil {
				rows[i].Status = "available"
			}
		}
	}
	return rows
}
