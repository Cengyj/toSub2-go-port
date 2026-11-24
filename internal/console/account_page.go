package console

import (
	"net/http"
)

type accountPageQuery struct {
	Emails         []string
	FilterActive   bool
	Page, PageSize int
}

// GET polling and POST email filtering consume one consistent account
// snapshot and one projection per matched account. History and secrets are
// never copied into this read model. Discovery is local and rate limited.
func (s *Server) sendAccountPage(w http.ResponseWriter, r *http.Request, q accountPageQuery) {
	mode, specified := r.URL.Query()["selection"]
	if specified && (len(mode) != 1 || mode[0] != "compact") {
		writeError(w, http.StatusBadRequest, "selection 仅支持单个 compact 值；省略时保留完整选择索引")
		return
	}
	compact := specified
	s.syncOutputs(r.Context(), false)
	list := s.jobs.PublicAccountsFiltered(q.Emails)
	matched := list.Accounts
	stats := map[string]int{"active": list.Stats.Active, "queued": list.Stats.Queued,
		"completed": list.Stats.Completed, "available": list.Stats.Available}
	if q.PageSize < 1 || q.PageSize > 100 {
		q.PageSize = 20
	}
	pages := max(1, (len(matched)+q.PageSize-1)/q.PageSize)
	q.Page = min(max(q.Page, 1), pages)
	start := (q.Page - 1) * q.PageSize
	end := min(start+q.PageSize, len(matched))
	visible := make([]map[string]any, 0, end-start)
	var fullSelection []map[string]any
	var compactSelection []accountSelection
	if compact {
		compactSelection = make([]accountSelection, 0, len(matched))
	} else {
		fullSelection = make([]map[string]any, 0, len(matched))
	}
	ids := make([]string, 0, len(matched))
	for _, item := range matched {
		ids = append(ids, item.Job.ID)
	}
	totp := s.jobs.TOTPListStates(ids)
	for i, item := range matched {
		view := s.publicAccount(item, totp[item.Job.ID])
		onPage := i >= start && i < end
		if onPage {
			visible = append(visible, view)
		}
		if compact {
			compactSelection = append(compactSelection, compactAccountSelection(view))
			continue
		}
		// Only a visible row shares its map with the full row response. Off-page
		// rows can transfer ownership instead of allocating an identical copy.
		selected := view
		if onPage {
			selected = make(map[string]any, len(view))
			for key, value := range view {
				selected[key] = value
			}
		}
		for _, key := range []string{"prompt", "updatedAt", "lastError", "logs"} {
			delete(selected, key)
		}
		fullSelection = append(fullSelection, selected)
	}
	response := map[string]any{"jobs": visible, "total": len(matched), "page": q.Page, "pageSize": q.PageSize, "pages": pages,
		"pagination": map[string]any{"page": q.Page, "pageSize": q.PageSize, "total": len(matched), "totalPages": pages, "totalAll": list.Total},
		"stats":      stats, "selection": fullSelection, "filter": map[string]any{"active": q.FilterActive, "requested": len(q.Emails), "matched": len(matched)}}
	if compact {
		response["selection"], response["selectionMode"] = compactSelection, "compact"
	}
	writeJSON(w, http.StatusOK, response)
}
