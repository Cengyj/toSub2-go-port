package console

import (
	"errors"
	"net/http"
	"net/url"

	"tosub2-go-port/internal/protocol"
)

func (s *Server) accountQualityModelsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "模型目录仅支持读取")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	ids, valid := qualityRequestIDs([]string{query.Get("id")})
	if err != nil || len(query) != 1 || len(query["id"]) != 1 || !valid || len(ids) != 1 {
		writeError(w, http.StatusBadRequest, "请选择一个模型目录来源账号")
		return
	}
	catalog, err := s.quality.AccountModels(r.Context(), ids[0])
	if err == nil {
		writeJSON(w, http.StatusOK, catalog)
		return
	}
	var failure *protocol.AccountModelsError
	if !errors.As(err, &failure) {
		writeError(w, http.StatusBadGateway, "模型目录读取失败，请稍后重试")
		return
	}
	status := http.StatusBadGateway
	switch failure.Code {
	case "not_found":
		status = http.StatusNotFound
	case "busy", "superseded", "authorization_unavailable", "recovery_pending", "proxy_selection_required":
		status = http.StatusConflict
	case "authorization_expired", "unauthorized", "invalid_credentials":
		status = http.StatusUnauthorized
	case "forbidden":
		status = http.StatusForbidden
	case "rate_limited":
		status = http.StatusTooManyRequests
	case "timeout":
		status = http.StatusGatewayTimeout
	case "canceled":
		status = http.StatusRequestTimeout
	case "transport_unavailable", "unsafe_transport":
		status = http.StatusServiceUnavailable
	}
	out := map[string]any{"error": failure.Error(), "code": failure.Code}
	if failure.HTTPStatus != 0 {
		out["httpStatus"] = failure.HTTPStatus
	}
	writeJSON(w, status, out)
}
