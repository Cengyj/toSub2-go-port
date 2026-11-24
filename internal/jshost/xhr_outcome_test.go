package jshost

import "testing"

func TestXHRTimeoutDoesNotClassifyNextRequestFailure(t *testing.T) {
	h, r, output := testRealm(t, "https://fixture.test/")
	evalTest(t, r, `var outcomes=[],xhr=new XMLHttpRequest();
xhr.ontimeout=()=>outcomes.push('timeout');xhr.onerror=()=>outcomes.push('error');xhr.onabort=()=>outcomes.push('abort');xhr.onload=()=>outcomes.push('load');xhr.onloadend=()=>outcomes.push('end');
xhr.open('GET','/first');xhr.timeout=1;xhr.send();`)
	first := auditRequests(t, output)
	if len(first) != 1 {
		t.Fatal("first request not dispatched")
	}
	drainTasks(t, h)
	if got := evalTest(t, r, `JSON.stringify(outcomes)`).String(); got != `["timeout","end"]` {
		t.Fatal(got)
	}
	evalTest(t, r, `xhr.open('GET','/second');xhr.timeout=0;xhr.send();`)
	second := auditRequests(t, output)
	if len(second) != 1 {
		t.Fatal("second request not dispatched")
	}
	if err := h.receive(inputEvent{command: command{"id": second[0]["id"], "error": "synthetic-network-failure"}}); err != nil {
		t.Fatal(err)
	}
	drainTasks(t, h)
	if got := evalTest(t, r, `JSON.stringify({events:outcomes,state:xhr.readyState,status:xhr.status,url:xhr.responseURL,headers:xhr.getAllResponseHeaders()})`).String(); got != `{"events":["timeout","end","error","end"],"state":4,"status":0,"url":"","headers":""}` {
		t.Fatal("prior timeout contaminated the next operation", got)
	}
	// The first response arrives after both requests have ended. It cannot
	// publish a result or success event for either request.
	auditReply(t, h, first[0], "late-first-response")
	if got := evalTest(t, r, `JSON.stringify(outcomes)`).String(); got != `["timeout","end","error","end"]` {
		t.Fatal(got)
	}
}

func TestXHRZeroStatusCannotReportLoadSuccess(t *testing.T) {
	h, r, output := testRealm(t, "https://fixture.test/")
	evalTest(t, r, `var events=[],xhr=new XMLHttpRequest();xhr.open('GET','/failed');xhr.onload=()=>events.push('load');xhr.onerror=()=>events.push('error');xhr.onloadend=()=>events.push('end');xhr.send();`)
	requests := auditRequests(t, output)
	if len(requests) != 1 {
		t.Fatal("request not dispatched")
	}
	if err := h.receive(inputEvent{command: command{"id": requests[0]["id"], "status": float64(0), "body": "not-a-response", "headers": map[string]any{"x-private": "discard"}}}); err != nil {
		t.Fatal(err)
	}
	drainTasks(t, h)
	if got := evalTest(t, r, `JSON.stringify({events,status:xhr.status,text:xhr.responseText,headers:xhr.getAllResponseHeaders()})`).String(); got != `{"events":["error","end"],"status":0,"text":"","headers":""}` {
		t.Fatal("network failure masqueraded as load success", got)
	}
}
