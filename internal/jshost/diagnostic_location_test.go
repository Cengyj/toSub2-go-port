package jshost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticLocationUsesEngineStackWithoutReadingScriptStack(t *testing.T) {
	h, r, _ := testRealm(t, "https://fixture.invalid/")
	_, cause := r.vm.RunScript("https://private.invalid/"+privateDiagnosticFixture, `
var stackRead=false;
const failure=new Error("`+privateDiagnosticFixture+`");
Object.defineProperty(failure,"stack",{get(){stackRead=true;throw new Error("private stack getter")}});
throw failure;
//# sourceURL=https://private.invalid/secret-source
`)
	if cause == nil {
		t.Fatal("fixture did not throw")
	}
	err := h.scriptErrorAt(cause, "timer_callback")
	var diagnostic *Error
	if !errors.As(err, &diagnostic) || diagnostic.Location == nil || !ValidDiagnosticLocation(diagnostic.Location) {
		t.Fatal("engine exception lost its bounded numeric location")
	}
	if r.vm.Get("stackRead").ToBoolean() {
		t.Fatal("diagnostics evaluated a script-controlled stack getter")
	}
	want := fmt.Sprintf("GO_JS_RUNTIME: SCRIPT_EXCEPTION [phase=timer_callback] [line=%d column=%d]", diagnostic.Location.Line, diagnostic.Location.Column)
	if err.Error() != want {
		t.Fatal("diagnostic included data other than safe code, phase and coordinates")
	}
	wrapped := fmt.Errorf("%s: %w", privateDiagnosticFixture, err)
	outer := h.scriptErrorAt(wrapped, "scheduled_callback")
	var preserved *Error
	if !errors.As(outer, &preserved) || preserved.Location != diagnostic.Location || preserved.Phase != "timer_callback" || !errors.Is(outer, cause) || outer.Error() != want {
		t.Fatal("wrapping changed the original location or exposed private context")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	h.ctx = canceled
	if got := h.scriptErrorAt(wrapped, "timer_callback"); !errors.Is(got, context.Canceled) || strings.Contains(got.Error(), "line=") {
		t.Fatal("location displaced cancellation")
	}
}

func TestDiagnosticLocationNeverFormatsSourceMapNames(t *testing.T) {
	h, r, _ := testRealm(t, "https://fixture.invalid/")
	// Sobek may apply source maps to native positions. Only their validated
	// numeric coordinates may cross this boundary, never mapped names or URLs.
	sourceMap := `{"version":3,"sources":["https://private.invalid/` + privateDiagnosticFixture + `"],"names":["private-function"],"mappings":"AAACA"}`
	_, cause := r.vm.RunScript("private-original.js", `throw new Error("`+privateDiagnosticFixture+`");
//# sourceMappingURL=data:application/json;base64,`+base64.StdEncoding.EncodeToString([]byte(sourceMap)))
	if cause == nil {
		t.Fatal("fixture did not throw")
	}
	err := h.scriptErrorAt(cause, "initial_script")
	var diagnostic *Error
	if !errors.As(err, &diagnostic) || diagnostic.Location == nil || !ValidDiagnosticLocation(diagnostic.Location) {
		t.Fatal("source-map fixture lost its valid numeric location")
	}
	data, _ := json.Marshal(failureEvent(diagnostic))
	for _, private := range []string{privateDiagnosticFixture, "private.invalid", "private-function", "private-original", "sourceMappingURL"} {
		if strings.Contains(err.Error(), private) || strings.Contains(string(data), private) {
			t.Fatal("source-map text escaped the diagnostic")
		}
	}
}

func TestDiagnosticLocationBoundsAndWorkerProtocol(t *testing.T) {
	for _, location := range []*DiagnosticLocation{
		{Line: 0, Column: 1}, {Line: 1, Column: 0}, {Line: -1, Column: 1},
		{Line: 1, Column: -1}, {Line: maxDiagnosticCoordinate + 1, Column: 1},
		{Line: 1, Column: maxDiagnosticCoordinate + 1},
	} {
		if ValidDiagnosticLocation(location) {
			t.Fatal("invalid diagnostic coordinate accepted")
		}
		diagnostic := &Error{Code: "SCRIPT_EXCEPTION", Location: location}
		if diagnostic.Error() != "GO_JS_RUNTIME: SCRIPT_EXCEPTION" || failureEvent(diagnostic)["location"] != nil {
			t.Fatal("invalid coordinate was serialized")
		}
		_, err := workerEventDiagnostic(command{"code": "SCRIPT_EXCEPTION", "location": location})
		var failure *Error
		if !errors.As(err, &failure) || failure.Code != "WORKER_PROTOCOL_INVALID" {
			t.Fatal("embedded worker accepted invalid coordinates")
		}
	}
	for _, raw := range []string{
		`{}`, `{"line":1}`, `{"column":1}`, `{"line":null,"column":1}`, `{"line":1,"column":null}`,
		`{"line":1.5,"column":1}`, `{"line":"1","column":1}`, `{"line":true,"column":1}`,
		`{"line":9999999999999999999999999,"column":1}`, `"private-location"`, `[]`,
	} {
		var location any
		if json.Unmarshal([]byte(raw), &location) != nil {
			t.Fatal("invalid fixture JSON")
		}
		if _, err := workerEventDiagnostic(command{"code": "SCRIPT_EXCEPTION", "location": location}); err == nil {
			t.Fatal("embedded worker accepted a malformed location")
		}
	}
	for _, location := range []*DiagnosticLocation{nil, {Line: 1, Column: maxDiagnosticCoordinate}} {
		got, err := workerEventDiagnostic(command{"code": "SCRIPT_EXCEPTION", "phase": "timer_callback", "location": location})
		if err != nil || got.Code != "SCRIPT_EXCEPTION" || !ValidDiagnosticLocation(got.Location) {
			t.Fatal("valid or legacy embedded-worker diagnostic rejected")
		}
	}
}

func TestTimerLocationSurvivesProcessWorker(t *testing.T) {
	path := diagnosticInput(t, `setTimeout(function(){
  const failure=new Error(__config.privateInput);
  Object.defineProperty(failure,'stack',{get(){throw new Error('private stack getter')}});
  throw failure;
},0);`)
	result := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "", "", true, 3*time.Second)
	expectDiagnosticFailure(t, result, 1, "SCRIPT_EXCEPTION", "timer_callback")
	var event struct {
		Location *DiagnosticLocation `json:"location"`
	}
	if json.Unmarshal([]byte(result.stdout), &event) != nil || event.Location == nil || !ValidDiagnosticLocation(event.Location) {
		t.Fatal("process worker lost timer location")
	}
}

func TestEmbeddedWorkerLocationSurvivesUnhandledFailure(t *testing.T) {
	path := diagnosticInput(t, `SentinelSDK={token:()=>new Promise(()=>{
  new Worker(URL.createObjectURL(new Blob(['\nthrow new Error('+JSON.stringify(__config.privateInput)+');'])));
})};`)
	result := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "", "{\"type\":\"call\",\"method\":\"token\",\"flow\":\"fixture\"}\n", true, 3*time.Second)
	expectDiagnosticFailure(t, result, 1, "SCRIPT_REJECTION", "promise_callback")
	var event struct {
		Location *DiagnosticLocation `json:"location"`
	}
	if json.Unmarshal([]byte(result.stdout), &event) != nil || event.Location == nil || event.Location.Line != 2 {
		t.Fatal("unhandled worker failure lost the originating engine location")
	}
}

func TestEmbeddedWorkerListenerFailureDoesNotInheritWorkerLocation(t *testing.T) {
	path := diagnosticInput(t, `SentinelSDK={token:()=>new Promise(()=>{
  const worker=new Worker(URL.createObjectURL(new Blob(['\nthrow new Error('+JSON.stringify(__config.privateInput)+');'])));
  worker.onerror=event=>{
    event.preventDefault();
    throw new Error(__config.privateInput);
  };
})};`)
	result := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "", "{\"type\":\"call\",\"method\":\"token\",\"flow\":\"fixture\"}\n", true, 3*time.Second)
	expectDiagnosticFailure(t, result, 1, "SCRIPT_REJECTION", "promise_callback")
	var event struct {
		Location *DiagnosticLocation `json:"location"`
	}
	if json.Unmarshal([]byte(result.stdout), &event) != nil || event.Location != nil {
		t.Fatal("listener rejection inherited the canceled worker failure's location")
	}
}
