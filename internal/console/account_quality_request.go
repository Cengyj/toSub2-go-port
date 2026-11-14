package console

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// Probe actions are explicit user submissions. Do not silently accept a second
// options object, a misspelled option, or duplicate account/selection fields.
func decodeQualityProbeJSON(w http.ResponseWriter, r *http.Request, out any, fields ...string) bool {
	var raw json.RawMessage
	if !decodeJSON(w, r, &raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		writeError(w, 400, "检测请求必须是完整对象")
		return false
	}
	allowed, seen := map[string]bool{}, map[string]bool{}
	for _, field := range fields {
		allowed[field] = true
	}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] || seen[key] {
			writeError(w, 400, "检测请求包含未知或重复字段")
			return false
		}
		seen[key] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			writeError(w, 400, "检测请求字段不能为 null")
			return false
		}
	}
	if _, err := decoder.Token(); err != nil || json.Unmarshal(raw, out) != nil {
		writeError(w, 400, "检测请求字段无效")
		return false
	}
	return true
}
