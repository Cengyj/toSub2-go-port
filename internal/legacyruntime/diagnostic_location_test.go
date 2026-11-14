package legacyruntime

import (
	"encoding/json"
	"errors"
	"testing"

	"tosub2-go-port/internal/jshost"
)

func TestRuntimeFailureLocationValidationAndCompatibility(t *testing.T) {
	for _, code := range []string{"SCRIPT_EXCEPTION", "EXECUTION_FAILED"} {
		err := runtimeFailure(runtimeMessage{Code: code})
		if err == nil || err.Error() != "GO_JS_RUNTIME: "+code {
			t.Fatal("legacy code-only runtime failure changed")
		}
	}
	for _, raw := range []string{
		`{"code":"SCRIPT_EXCEPTION","phase":"timer_callback","location":null}`,
		`{"code":"SCRIPT_EXCEPTION","phase":"timer_callback","location":{"line":2,"column":17}}`,
		`{"code":"SCRIPT_EXCEPTION","phase":"timer_callback"}`,
	} {
		var message runtimeMessage
		if err := json.Unmarshal([]byte(raw), &message); err != nil {
			t.Fatal(err)
		}
		err := runtimeFailure(message)
		var diagnostic *jshost.Error
		if !errors.As(err, &diagnostic) || diagnostic.Phase != "timer_callback" || diagnostic.Location != message.Location {
			t.Fatal("runtime lost optional coordinates or broke an old message")
		}
	}
	for _, raw := range []string{
		`{}`, `{"line":1}`, `{"column":1}`, `{"line":null,"column":1}`, `{"line":1,"column":null}`,
		`{"line":0,"column":1}`, `{"line":1,"column":0}`, `{"line":-1,"column":1}`,
		`{"line":1,"column":1000001}`, `{"line":1000001,"column":1}`,
		`{"line":1.5,"column":1}`, `{"line":"1","column":1}`,
		`{"line":9999999999999999999999999999,"column":1}`, `"private-location"`, `[]`,
	} {
		var message runtimeMessage
		if err := json.Unmarshal([]byte(`{"code":"SCRIPT_EXCEPTION","location":`+raw+`}`), &message); err == nil && !errors.Is(runtimeFailure(message), ErrProtocol) {
			t.Fatal("runtime accepted invalid numeric coordinates")
		}
	}
}
