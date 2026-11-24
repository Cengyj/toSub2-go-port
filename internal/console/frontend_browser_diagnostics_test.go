package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/grafana/sobek"
	"golang.org/x/net/html"
)

const protectedBrowserMarkerID = "protected-browser-diagnostics"

// Browser output can contain rendered secrets and upstream errors. Retain a
// bounded prefix for parsing only; never include either raw stream in failures.
type protectedBrowserOutput struct {
	data  []byte
	total int64
}

func (output *protectedBrowserOutput) Write(value []byte) (int, error) {
	const limit = 64 * 1024
	output.total += int64(len(value))
	if remaining := limit - len(output.data); remaining > 0 {
		output.data = append(output.data, value[:min(len(value), remaining)]...)
	}
	return len(value), nil
}

func (output *protectedBrowserOutput) summary() string {
	return fmt.Sprintf("bytes=%d retained=%d truncated=%t", output.total, len(output.data), output.total > int64(len(output.data)))
}

type protectedBrowserEvent struct {
	Stage string `json:"stage"`
	AtMS  int    `json:"atMs"`
}

type protectedBrowserDiagnostics struct {
	Version          int                     `json:"version"`
	Status           string                  `json:"status"`
	Failure          string                  `json:"failure"`
	Events           []protectedBrowserEvent `json:"events"`
	FinishedAtMS     *int                    `json:"finishedAtMs"`
	LateEvents       []protectedBrowserEvent `json:"lateEvents"`
	Dialogs          int                     `json:"dialogs"`
	ScriptErrors     int                     `json:"scriptErrors"`
	PromiseErrors    int                     `json:"promiseErrors"`
	RejectedMessages int                     `json:"rejectedMessages"`
}

var protectedBrowserStages = map[string]bool{
	"driver_started": true, "settings_checked": true, "maintenance_checked": true,
	"trash_checked": true, "monitor_checked": true, "preview_requested": true,
	"iframe_mounted": true, "iframe_loaded": true, "script_started": true,
	"parent_access_denied": true, "parent_access_allowed": true,
	"csp_connect_blocked": true, "fetch_rejected": true, "fetch_resolved": true,
	"final_message": true, "preview_checked": true, "download_checked": true,
}

func decodeProtectedBrowserDiagnostics(raw []byte) (protectedBrowserDiagnostics, bool) {
	var report protectedBrowserDiagnostics
	if len(raw) == 0 || len(raw) > 4096 {
		return report, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&report) != nil || decoder.Decode(new(any)) != io.EOF {
		return protectedBrowserDiagnostics{}, false
	}
	if report.Version != 1 || len(report.Events)+len(report.LateEvents) > len(protectedBrowserStages) || report.Dialogs < 0 || report.Dialogs > 9 {
		return protectedBrowserDiagnostics{}, false
	}
	if report.Status != "running" && report.Status != "passed" && report.Status != "failed" {
		return protectedBrowserDiagnostics{}, false
	}
	if report.Failure != "" && report.Failure != "assertion" && report.Failure != "wait_timeout" && report.Failure != "script_exception" {
		return protectedBrowserDiagnostics{}, false
	}
	if (report.Status == "failed") != (report.Failure != "") {
		return protectedBrowserDiagnostics{}, false
	}
	if (report.Status == "running") != (report.FinishedAtMS == nil) || report.FinishedAtMS == nil && len(report.LateEvents) != 0 {
		return protectedBrowserDiagnostics{}, false
	}
	if report.FinishedAtMS != nil && (*report.FinishedAtMS < 0 || *report.FinishedAtMS > 30000) {
		return protectedBrowserDiagnostics{}, false
	}
	for _, count := range []int{report.ScriptErrors, report.PromiseErrors, report.RejectedMessages} {
		if count < 0 || count > 100 {
			return protectedBrowserDiagnostics{}, false
		}
	}
	seen, previous := map[string]bool{}, 0
	for _, event := range report.Events {
		if !protectedBrowserStages[event.Stage] || seen[event.Stage] || event.AtMS < previous || event.AtMS > 30000 {
			return protectedBrowserDiagnostics{}, false
		}
		if report.FinishedAtMS != nil && event.AtMS > *report.FinishedAtMS {
			return protectedBrowserDiagnostics{}, false
		}
		seen[event.Stage], previous = true, event.AtMS
	}
	if report.FinishedAtMS != nil {
		previous = *report.FinishedAtMS
	}
	for _, event := range report.LateEvents {
		if !protectedBrowserStages[event.Stage] || seen[event.Stage] || event.AtMS < previous || event.AtMS > 30000 {
			return protectedBrowserDiagnostics{}, false
		}
		seen[event.Stage], previous = true, event.AtMS
	}
	return report, true
}

func protectedBrowserDOMDiagnostics(output []byte) (protectedBrowserDiagnostics, string) {
	parser := html.NewTokenizer(bytes.NewReader(output))
	var raw []byte
	inside, found := false, false
	for {
		switch parser.Next() {
		case html.ErrorToken:
			if parser.Err() != io.EOF || inside {
				return protectedBrowserDiagnostics{}, "invalid"
			}
			if !found {
				return protectedBrowserDiagnostics{}, "missing"
			}
			report, valid := decodeProtectedBrowserDiagnostics(raw)
			if !valid {
				return protectedBrowserDiagnostics{}, "invalid"
			}
			return report, "valid"
		case html.StartTagToken:
			token := parser.Token()
			if token.Data != "script" {
				continue
			}
			for _, attr := range token.Attr {
				if attr.Key == "id" && attr.Val == protectedBrowserMarkerID {
					if found {
						return protectedBrowserDiagnostics{}, "invalid"
					}
					inside, found = true, true
				}
			}
		case html.TextToken:
			if inside {
				raw = append(raw, parser.Text()...)
				if len(raw) > 4096 {
					return protectedBrowserDiagnostics{}, "invalid"
				}
			}
		case html.EndTagToken:
			if parser.Token().Data == "script" {
				inside = false
			}
		}
	}
}

func (report protectedBrowserDiagnostics) previewObservation() string {
	seen := map[string]bool{}
	for _, event := range report.Events {
		seen[event.Stage] = true
	}
	switch {
	case !seen["preview_requested"]:
		return "not_reached"
	case !seen["iframe_mounted"]:
		return "mount_not_observed"
	case !seen["script_started"]:
		return "script_start_not_observed"
	case seen["parent_access_allowed"] || seen["fetch_resolved"]:
		return "isolation_rejected"
	case !seen["fetch_rejected"]:
		return "fetch_settlement_not_observed"
	case !seen["final_message"]:
		return "final_message_not_observed"
	case !seen["csp_connect_blocked"]:
		return "connect_src_rejection_not_observed"
	case !seen["parent_access_denied"]:
		return "parent_access_rejection_not_observed"
	default:
		return "isolation_observed"
	}
}

func (report protectedBrowserDiagnostics) originalIsolationObserved() bool {
	// CSP events are asynchronous observations. The original behavioral
	// assertions must succeed before finish; late events cannot supply them.
	seen := map[string]bool{}
	for _, event := range report.Events {
		seen[event.Stage] = true
	}
	return seen["preview_requested"] && seen["iframe_mounted"] && seen["script_started"] && seen["parent_access_denied"] && seen["fetch_rejected"] && seen["final_message"] && !seen["parent_access_allowed"] && !seen["fetch_resolved"]
}

func protectedBrowserEvidence(stdout, stderr *protectedBrowserOutput) string {
	report, marker := protectedBrowserDOMDiagnostics(stdout.data)
	// Classify a small set of process diagnostics without returning any raw text,
	// URLs, paths, stack traces, DOM content or command arguments.
	var categories []string
	lower := strings.ToLower(string(stderr.data))
	for _, word := range []string{"fatal", "crash", "sandbox", "gpu", "network", "error"} {
		if strings.Contains(lower, word) {
			categories = append(categories, word)
		}
	}
	base := fmt.Sprintf("stdout={%s} stderr={%s categories=%v} marker=%s", stdout.summary(), stderr.summary(), categories, marker)
	if marker != "valid" {
		return base
	}
	// Only a validated typed report is rendered, never the original marker text.
	encoded, _ := json.Marshal(report)
	latest := report
	latest.Events = append(append([]protectedBrowserEvent{}, report.Events...), report.LateEvents...)
	return fmt.Sprintf("%s preview_at_finish=%s preview_latest=%s diagnostics=%s", base, report.previewObservation(), latest.previewObservation(), encoded)
}

// These times are observations in the parent renderer, not assertions about
// when another renderer executed. Instrumentation adds no waits or HTTP calls.
const protectedBrowserDiagnosticsDriver = `
function createProtectedBrowserDiagnostics(host, marker) {
 const allowed=new Set(["driver_started","settings_checked","maintenance_checked","trash_checked","monitor_checked","preview_requested","iframe_mounted","iframe_loaded","script_started","parent_access_denied","parent_access_allowed","csp_connect_blocked","fetch_rejected","fetch_resolved","final_message","preview_checked","download_checked"]);
 const started=host.performance.now(),events=[],lateEvents=[],seen=new Set();let finishedAtMs=null,frozen=null;
 let status="running",failure="",scriptErrors=0,promiseErrors=0,rejectedMessages=0;
 const stamp=()=>Math.max(lateEvents.at(-1)?.atMs||events.at(-1)?.atMs||0,finishedAtMs||0,Math.min(30000,Math.max(0,Math.floor(host.performance.now()-started))));
 const current=()=>({version:1,status,failure,events:events.map(event=>({...event})),finishedAtMs,dialogs:Math.min(9,host.document.querySelectorAll('[role="dialog"]').length),scriptErrors,promiseErrors,rejectedMessages});
 const snapshot=()=>({...frozen||current(),events:(frozen?.events||events).map(event=>({...event})),lateEvents:lateEvents.map(event=>({...event}))});
 const publish=()=>{marker.textContent=JSON.stringify(snapshot());};
 const record=stage=>{if(!allowed.has(stage)||seen.has(stage))return;seen.add(stage);(frozen?lateEvents:events).push({stage,atMs:stamp()});publish();};
 const scriptError=()=>{if(!frozen){scriptErrors=Math.min(100,scriptErrors+1);publish();}};
 const promiseError=()=>{if(!frozen){promiseErrors=Math.min(100,promiseErrors+1);publish();}};
 host.addEventListener("error",scriptError);host.addEventListener("unhandledrejection",promiseError);
 record("driver_started");
 return {record,snapshot,has:stage=>seen.has(stage),rejectMessage(){if(!frozen){rejectedMessages=Math.min(100,rejectedMessages+1);publish();}},finish(next,reason=""){if(frozen)return;status=next;failure=reason;finishedAtMs=stamp();frozen=current();publish();},dispose(){host.removeEventListener("error",scriptError);host.removeEventListener("unhandledrejection",promiseError);}};
}
function observeProtectedPreviewMessage(event,frame,channel,diagnostics) {
 const data=event.data;
 if(!data||data.fixture!=="opaque-preview")return null;
 const allowed=new Set(["script_started","parent_access_denied","parent_access_allowed","csp_connect_blocked","fetch_rejected","fetch_resolved","final_message"]);
 if(!frame||event.source!==frame.contentWindow||event.origin!=="null"||data.channel!==channel||!allowed.has(data.stage)||data.stage==="final_message"&&(typeof data.blocked!=="boolean"||typeof data.networkFailed!=="boolean")){diagnostics.rejectMessage();return null;}
 diagnostics.record(data.stage);
 return data.stage==="final_message"?{blocked:data.blocked,networkFailed:data.networkFailed}:null;
}
class ProtectedBrowserAssertion extends Error {
 constructor(message,kind="assertion"){super(message);this.fixtureKind=kind;}
}
function protectedBrowserFailure(error) {
 return error instanceof ProtectedBrowserAssertion?{failure:error.fixtureKind,label:error.message}:{failure:"script_exception",label:"fixture script exception (details suppressed)"};
}
`

func protectedBrowserTestReport(stages ...string) protectedBrowserDiagnostics {
	finishedAtMS := len(stages) * 25
	report := protectedBrowserDiagnostics{Version: 1, Status: "failed", Failure: "wait_timeout", Events: []protectedBrowserEvent{}, FinishedAtMS: &finishedAtMS, LateEvents: []protectedBrowserEvent{}}
	for index, stage := range stages {
		report.Events = append(report.Events, protectedBrowserEvent{Stage: stage, AtMS: index * 25})
	}
	return report
}

func protectedBrowserTestMarker(t *testing.T, report protectedBrowserDiagnostics) []byte {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Appendf(nil, `<script id="%s" type="application/json">%s</script>`, protectedBrowserMarkerID, raw)
}

func TestProtectedBrowserDiagnosticsDistinguishMissingObservations(t *testing.T) {
	for _, test := range []struct {
		want   string
		stages []string
	}{
		{"not_reached", []string{"driver_started"}},
		{"mount_not_observed", []string{"preview_requested"}},
		{"script_start_not_observed", []string{"preview_requested", "iframe_mounted", "iframe_loaded"}},
		{"fetch_settlement_not_observed", []string{"preview_requested", "iframe_mounted", "script_started", "parent_access_denied"}},
		{"final_message_not_observed", []string{"preview_requested", "iframe_mounted", "script_started", "parent_access_denied", "fetch_rejected", "csp_connect_blocked"}},
		{"connect_src_rejection_not_observed", []string{"preview_requested", "iframe_mounted", "script_started", "parent_access_denied", "fetch_rejected", "final_message"}},
		{"isolation_rejected", []string{"preview_requested", "iframe_mounted", "script_started", "fetch_resolved"}},
		{"isolation_observed", []string{"preview_requested", "iframe_mounted", "script_started", "parent_access_denied", "fetch_rejected", "final_message", "csp_connect_blocked"}},
	} {
		t.Run(test.want, func(t *testing.T) {
			report, state := protectedBrowserDOMDiagnostics(protectedBrowserTestMarker(t, protectedBrowserTestReport(test.stages...)))
			if state != "valid" || report.previewObservation() != test.want {
				t.Fatalf("state=%s observation=%s want=%s", state, report.previewObservation(), test.want)
			}
		})
	}
}

func TestProtectedBrowserDiagnosticsBoundAndRedactOutput(t *testing.T) {
	const private = "SYNTHETIC_PRIVATE_PASSWORD_TOKEN_BODY_7d9a"
	stdout, stderr := &protectedBrowserOutput{}, &protectedBrowserOutput{}
	marker := protectedBrowserTestMarker(t, protectedBrowserTestReport("preview_requested", "iframe_mounted"))
	input := append(marker, []byte(`<div data-token="`+private+`">`+strings.Repeat(private, 10000)+`</div>`)...)
	if n, err := stdout.Write(input); err != nil || n != len(input) {
		t.Fatalf("bounded output did not drain the full write: n=%d err=%v", n, err)
	}
	if _, err := stderr.Write([]byte("ERROR: " + strings.Repeat(private, 10000))); err != nil {
		t.Fatal(err)
	}
	if len(stdout.data) != 64*1024 || len(stderr.data) != 64*1024 || stdout.total != int64(len(input)) {
		t.Fatal("browser output exceeded its fixed retention limit or lost byte accounting")
	}
	evidence := protectedBrowserEvidence(stdout, stderr)
	for _, fragment := range []string{"truncated=true", "categories=[error]", "marker=valid", "preview_at_finish=script_start_not_observed"} {
		if !strings.Contains(evidence, fragment) {
			t.Fatalf("safe evidence omitted %q", fragment)
		}
	}
	if strings.Contains(evidence, private) || strings.Contains(evidence, "data-token") || len(evidence) > 4096 {
		t.Fatal("browser evidence leaked raw/private content or exceeded its bound")
	}
}

func TestProtectedBrowserDiagnosticsRejectMalformedAndPrivateMarkers(t *testing.T) {
	const private = "SYNTHETIC_PRIVATE_DIAGNOSTIC_4e32"
	valid := protectedBrowserTestMarker(t, protectedBrowserTestReport("driver_started"))
	for name, value := range map[string][]byte{
		"missing":          []byte("<html><body>" + private + "</body></html>"),
		"duplicate":        append(append([]byte{}, valid...), valid...),
		"truncated":        valid[:len(valid)-5],
		"invalid_json":     []byte(`<script id="` + protectedBrowserMarkerID + `">{` + private + `}</script>`),
		"oversized":        []byte(`<script id="` + protectedBrowserMarkerID + `">` + strings.Repeat(private, 200) + `</script>`),
		"unknown_field":    bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":1,"secret":"`+private+`"`), 1),
		"unknown_stage":    bytes.Replace(valid, []byte(`"driver_started"`), []byte(`"`+private+`"`), 1),
		"unknown_status":   bytes.Replace(valid, []byte(`"failed"`), []byte(`"`+private+`"`), 1),
		"negative_time":    bytes.Replace(valid, []byte(`"atMs":0`), []byte(`"atMs":-1`), 1),
		"unbounded_time":   bytes.Replace(valid, []byte(`"atMs":0`), []byte(`"atMs":30001`), 1),
		"duplicate_stage":  protectedBrowserTestMarker(t, protectedBrowserTestReport("driver_started", "driver_started")),
		"wrong_version":    bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":2`), 1),
		"failed_no_reason": bytes.Replace(valid, []byte(`"wait_timeout"`), []byte(`""`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr := &protectedBrowserOutput{}, &protectedBrowserOutput{}
			_, _ = stdout.Write(value)
			if _, state := protectedBrowserDOMDiagnostics(stdout.data); state == "valid" {
				t.Fatal("malformed or unexpected diagnostics were accepted")
			}
			if evidence := protectedBrowserEvidence(stdout, stderr); strings.Contains(evidence, private) || strings.Contains(evidence, "diagnostics=") {
				t.Fatal("invalid marker was rendered as trusted evidence")
			}
		})
	}
}

func TestProtectedBrowserDiagnosticsObserveOnlyCurrentOpaqueFrame(t *testing.T) {
	vm := sobek.New()
	frontendPollingRun(t, vm, protectedBrowserDiagnosticsDriver)
	frontendPollingRun(t, vm, `
function assert(value,message){if(!value)throw new Error(message);}
let now=10;const listeners=new Map(),marker={textContent:""};
const host={performance:{now:()=>now},document:{querySelectorAll:()=>[]},addEventListener:(name,fn)=>listeners.set(name,fn),removeEventListener:(name,fn)=>{assert(listeners.get(name)===fn,"listener mismatch");listeners.delete(name);}};
const diagnostics=createProtectedBrowserDiagnostics(host,marker),frame={contentWindow:{}},channel="synthetic-channel";
const event=stage=>({source:frame.contentWindow,origin:"null",data:{fixture:"opaque-preview",channel,stage}});
assert(observeProtectedPreviewMessage({data:{type:"pelican:size"}},frame,channel,diagnostics)===null,"production size message became a receipt");
for(const altered of [
 {...event("script_started"),source:{}},
 {...event("script_started"),origin:"https://SYNTHETIC_PRIVATE.invalid"},
 {...event("script_started"),data:{...event("script_started").data,channel:"stale-channel"}},
 {...event("script_started"),data:{...event("script_started").data,stage:"SYNTHETIC_PRIVATE"}},
 event("final_message")
])assert(observeProtectedPreviewMessage(altered,frame,channel,diagnostics)===null,"foreign or malformed message accepted");
assert(diagnostics.snapshot().events.length===1&&diagnostics.snapshot().rejectedMessages===5,"rejected event fabricated a stage");
for(const stage of ["script_started","parent_access_denied","fetch_rejected","csp_connect_blocked"]){now+=25;assert(observeProtectedPreviewMessage(event(stage),frame,channel,diagnostics)===null,"intermediate stage became final receipt");}
const final={...event("final_message"),data:{...event("final_message").data,blocked:true,networkFailed:true,secret:"SYNTHETIC_PRIVATE"}};
const receipt=observeProtectedPreviewMessage(final,frame,channel,diagnostics);
assert(receipt.blocked&&receipt.networkFailed&&Object.keys(receipt).length===2,"final receipt lost its booleans or copied arbitrary fields");
assert(observeProtectedPreviewMessage(final,{contentWindow:{}},channel,diagnostics)===null,"replaced iframe accepted a late receipt");
now=40000;diagnostics.record("preview_checked");now=-100;diagnostics.record("download_checked");diagnostics.record("SYNTHETIC_PRIVATE");
for(let index=0;index<150;index++){listeners.get("error")({message:"SYNTHETIC_PRIVATE"});listeners.get("unhandledrejection")({reason:new Error("SYNTHETIC_PRIVATE")});diagnostics.rejectMessage();}
const exception=protectedBrowserFailure(new Error("SYNTHETIC_PRIVATE"));
assert(exception.failure==="script_exception"&&!exception.label.includes("SYNTHETIC_PRIVATE"),"raw script exception leaked");
const assertion=protectedBrowserFailure(new ProtectedBrowserAssertion("static fixture timeout","wait_timeout"));
assert(assertion.label==="static fixture timeout"&&assertion.failure==="wait_timeout","fixture failure classification lost");
diagnostics.finish("failed",assertion.failure);
assert(!marker.textContent.includes("SYNTHETIC_PRIVATE")&&!marker.textContent.includes(channel),"private event data leaked into marker");
assert(diagnostics.snapshot().scriptErrors===100&&diagnostics.snapshot().promiseErrors===100&&diagnostics.snapshot().rejectedMessages===100,"diagnostic counters unbounded");
diagnostics.dispose();assert(listeners.size===0,"diagnostic listeners leaked");
globalThis.collectedDiagnostics=marker.textContent;
`)
	if _, valid := decodeProtectedBrowserDiagnostics([]byte(vm.Get("collectedDiagnostics").String())); !valid {
		t.Fatal("JavaScript observer emitted diagnostics outside the Go schema")
	}
}

func TestProtectedBrowserDiagnosticsFreezeFailureBeforeLateEvidence(t *testing.T) {
	vm := sobek.New()
	frontendPollingRun(t, vm, protectedBrowserDiagnosticsDriver)
	frontendPollingRun(t, vm, `
function assert(value,message){if(!value)throw new Error(message);}
let now=0;const marker={textContent:""},listeners=new Map();
const host={performance:{now:()=>now},document:{querySelectorAll:()=>[]},addEventListener:(name,fn)=>listeners.set(name,fn),removeEventListener:(name,fn)=>listeners.delete(name)};
const diagnostics=createProtectedBrowserDiagnostics(host,marker);
for(const stage of ["preview_requested","iframe_mounted","script_started","parent_access_denied"]){now+=25;diagnostics.record(stage);}
now=6000;diagnostics.finish("failed","wait_timeout");const frozen=diagnostics.snapshot();
for(const stage of ["fetch_rejected","final_message","csp_connect_blocked"]){now+=25;diagnostics.record(stage);}
listeners.get("error")({message:"SYNTHETIC_PRIVATE"});diagnostics.rejectMessage();diagnostics.finish("passed");
const latest=diagnostics.snapshot();
assert(latest.status==="failed"&&latest.failure==="wait_timeout"&&latest.finishedAtMs===6000,"late event changed terminal failure");
assert(JSON.stringify(latest.events)===JSON.stringify(frozen.events)&&latest.scriptErrors===0&&latest.rejectedMessages===0,"terminal diagnostic facts changed after freeze");
assert(latest.lateEvents.length===3&&latest.lateEvents[0].atMs===6025,"late stages were discarded or merged into original evidence");
globalThis.collectedDiagnostics=marker.textContent;
`)
	report, valid := decodeProtectedBrowserDiagnostics([]byte(vm.Get("collectedDiagnostics").String()))
	if !valid || report.previewObservation() != "fetch_settlement_not_observed" || report.originalIsolationObserved() {
		t.Fatal("late evidence replaced the original failed observation")
	}
	stdout, stderr := &protectedBrowserOutput{}, &protectedBrowserOutput{}
	_, _ = stdout.Write(protectedBrowserTestMarker(t, report))
	evidence := protectedBrowserEvidence(stdout, stderr)
	if !strings.Contains(evidence, "preview_at_finish=fetch_settlement_not_observed preview_latest=isolation_observed") {
		t.Fatal("evidence did not distinguish failure time from late observations")
	}
}
