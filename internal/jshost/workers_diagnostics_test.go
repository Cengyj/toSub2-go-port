package jshost

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWorkerWrappedDiagnosticCodesStayPrivate(t *testing.T) {
	for _, fixture := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%s: %w", privateDiagnosticFixture, &Error{Code: "SCRIPT_TYPEERROR", Phase: "worker_script"}), "SCRIPT_TYPEERROR"},
		{codeError("SCRIPT_RANGEERROR", errors.New(privateDiagnosticFixture)), "SCRIPT_RANGEERROR"},
		{&Error{Code: privateDiagnosticFixture}, "WORKER_SCRIPT_EXCEPTION"},
		{fmt.Errorf("%s: %w", privateDiagnosticFixture, &Error{Code: "SCRIPT_TYPEERROR\n" + privateDiagnosticFixture}), "WORKER_SCRIPT_EXCEPTION"},
		{errors.New(privateDiagnosticFixture), "WORKER_SCRIPT_EXCEPTION"},
	} {
		if got := safeWorkerCode(fixture.err); got != fixture.want {
			t.Fatalf("worker lost a safe diagnostic or exposed its cause: %q", got)
		}
	}
}

func TestWorkerTypeErrorListenersKeepClassificationAndPrivacy(t *testing.T) {
	for _, fixture := range []struct{ name, source string }{
		{"initial-script", `throw new TypeError("` + privateDiagnosticFixture + `");`},
		{"timer-callback", `setTimeout(()=>{throw new TypeError("` + privateDiagnosticFixture + `");},0);`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			source := `SentinelSDK={token:()=>new Promise(resolve=>{
  const worker=new Worker(URL.createObjectURL(new Blob([` + strconv.Quote(fixture.source) + `])));
  const events=[];
  worker.addEventListener('error',e=>{
    e.preventDefault();
    events.push({listener:'event',message:e.message,trusted:e.isTrusted,cancelable:e.cancelable});
  });
  worker.onerror=e=>{
    events.push({listener:'property',message:e.message,prevented:e.defaultPrevented});
    setTimeout(()=>resolve(JSON.stringify({events,continued:true})),0);
  };
})};`
			value := runSDKFixture(t, source, nil)
			if strings.Contains(value, privateDiagnosticFixture) {
				t.Fatal("worker error listener received private exception text")
			}
			want := `{"events":[{"listener":"event","message":"SCRIPT_TYPEERROR","trusted":true,"cancelable":true},{"listener":"property","message":"SCRIPT_TYPEERROR","prevented":true}],"continued":true}`
			if value != want {
				t.Fatalf("worker error classification or listener behavior changed: %s", value)
			}
		})
	}
}

func TestUnhandledWorkerTypeErrorRemainsFatalAndPrivate(t *testing.T) {
	path := diagnosticInput(t, `SentinelSDK={token:()=>new Promise(()=>{
  new Worker(URL.createObjectURL(new Blob(['throw new TypeError('+JSON.stringify(__config.privateInput)+');'])));
})};`)
	result := runDiagnosticWorker(t, []string{"--auth-js-worker", "sentinel", path}, "", "{\"type\":\"call\",\"method\":\"token\",\"flow\":\"fixture\"}\n", true, 3*time.Second)
	expectDiagnosticFailure(t, result, 1, "SCRIPT_REJECTION", "promise_callback")
}
