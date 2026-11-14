package jshost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNestedInlineScriptDiagnosticKeepsCaptureBoundary(t *testing.T) {
	// Nested DOM insertion has always converted its inner failure to a
	// catchable TypeError. Phase describes the host boundary that captures it,
	// not an inner location reconstructed from a script-controlled message.
	inner := `throw new Error("` + privateDiagnosticFixture + ` [phase=timer_callback]");`
	insertion := `const script=document.createElement('script');script.textContent=` + strconv.Quote(inner) + `;document.head.appendChild(script);`
	t.Run("caught-error-remains-catchable", func(t *testing.T) {
		value := runSDKFixture(t, `SentinelSDK={token:()=>{
  let caught;
  try {`+insertion+`} catch(e) {caught={type:e instanceof TypeError,name:e.name,message:e.message,hasPhase:'phase' in e};}
  return JSON.stringify({caught,continued:true});
}};`, nil)
		if strings.Contains(value, privateDiagnosticFixture) || strings.Contains(value, "phase=") || strings.Contains(value, "timer_callback") {
			t.Fatal("nested exception exposed private text or host execution phase")
		}
		want := `{"caught":{"type":true,"name":"TypeError","message":"GO_JS_RUNTIME: SCRIPT_EXCEPTION","hasPhase":false},"continued":true}`
		if value != want {
			t.Fatalf("nested script catch behavior changed: %s", value)
		}
	})
	for _, fixture := range []struct{ name, source, code string }{
		{"uncaught-nested-error", insertion, "SCRIPT_TYPEERROR"},
		{"direct-message-cannot-forge-phase", inner, "SCRIPT_EXCEPTION"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{
				"url":  "https://fixture.invalid/" + privateDiagnosticFixture,
				"html": "<!doctype html><html><head><script>" + fixture.source + "</script></head><body></body></html>",
			})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "private-input.json")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			result := runDiagnosticWorker(t, []string{"--auth-js-worker", "parent", path}, "", "", false, 3*time.Second)
			expectDiagnosticFailure(t, result, 1, fixture.code, "initial_script")
		})
	}
}
