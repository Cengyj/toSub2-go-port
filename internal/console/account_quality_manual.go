package console

import (
	"encoding/json"
	"errors"
	"net/http"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/modelprobe"
)

func (s *Server) accountQualityManualAPI(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method == http.MethodGet && path == "/manual" {
		ids, ok := qualityRequestIDs([]string{r.URL.Query().Get("id")})
		if !ok || len(ids) != 1 {
			writeError(w, 400, "请选择手动检测账号")
			return
		}
		writeJSON(w, 200, s.quality.ManualProbe(ids[0]))
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var req struct {
		ID        string          `json:"id"`
		RequestID string          `json:"requestId"`
		Task      string          `json:"task,omitempty"`
		Options   json.RawMessage `json:"options"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
	}
	if !decodeQualityProbeJSON(w, r, &req, "id", "requestId", "task", "options") {
		return
	}
	selected, optionsErr := accountquality.DecodeRunOptions(req.Options)
	if optionsErr != nil || path == "/manual/cancel" && len(req.Options) != 0 {
		writeError(w, 400, "请选择完整有效的单次模型与推理强度")
		return
	}
	ids, ok := qualityRequestIDs([]string{req.ID})
	if !ok || len(ids) != 1 || req.RequestID == "" {
		writeError(w, 400, "手动检测账号或请求标识无效")
		return
	}
	if path == "/manual/cancel" {
		record, err := s.quality.CancelManualProbe(r.Context(), req.ID, req.RequestID)
		if err != nil {
			writeError(w, qualityAPIStatus(err), err.Error())
			return
		}
		writeJSON(w, 200, record)
		return
	}
	if req.Task != modelprobe.PelicanTask {
		writeError(w, 400, "请选择鹈鹕骑自行车题目")
		return
	}
	if s.trashActionGate != nil {
		select {
		case s.trashActionGate <- struct{}{}:
			defer func() { <-s.trashActionGate }()
		case <-r.Context().Done():
			writeError(w, 503, "等待账号回收操作结束超时，未提交检测")
			return
		}
	}
	record, err := s.quality.StartManualProbeWithOptions(r.Context(), req.ID, req.RequestID, req.Task, selected)
	if err != nil {
		status := qualityAPIStatus(err)
		var gate *accountquality.GateError
		if errors.As(err, &gate) && (gate.Code == "cooldown" || gate.Code == "busy") {
			status = 409
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, record)
}
