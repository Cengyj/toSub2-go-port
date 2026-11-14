package jshost

import (
	"errors"
	"fmt"
	"testing"
)

func TestTimerStrictCallbacksReceiveTheirGlobalObject(t *testing.T) {
	for _, method := range []string{"setTimeout", "setInterval"} {
		t.Run(method, func(t *testing.T) {
			h, r, _ := testRealm(t, "https://timer.example.test/")
			evalTest(t, r, `var callbackResult;
var timerID = `+method+`(function(first, second) {
  "use strict";
  clearTimeout(timerID);
  callbackResult = {global: this === globalThis, first, same: second === timerArgument};
}, 0, "fixture-value", timerArgument = {fixture: true});`)
			if len(h.tasks) != 1 {
				t.Fatal("timer was not scheduled exactly once")
			}
			// Model the loop's repeat registration, allowing a callback to cancel
			// either timer kind with clearTimeout before it returns.
			task := h.tasks[0]
			h.tasks = nil
			if task.repeat > 0 {
				h.tasks = append(h.tasks, task)
			}
			if err := task.run(); err != nil {
				t.Fatal(err)
			}
			if len(h.tasks) != 0 {
				t.Fatal("timer callback could not cancel its repeat")
			}
			got := evalTest(t, r, `JSON.stringify(callbackResult)`).String()
			if got != `{"global":true,"first":"fixture-value","same":true}` {
				t.Fatalf("timer invocation changed browser callback semantics: %s", got)
			}
		})
	}
}

func TestTimerSourceIsConvertedBeforeRegistration(t *testing.T) {
	h, r, _ := testRealm(t, "https://timer.example.test/")
	evalTest(t, r, `var sourceValue="initial", timerResult, conversions=0;
var timerSource={toString(){conversions++; return "timerResult="+JSON.stringify(sourceValue)}};
setTimeout(timerSource, 0);
var conversionsBeforeCallback=conversions;
sourceValue="changed";`)
	drainTasks(t, h)
	got := evalTest(t, r, `JSON.stringify({timerResult,conversions,conversionsBeforeCallback})`).String()
	if got != `{"timerResult":"initial","conversions":1,"conversionsBeforeCallback":1}` {
		t.Fatalf("timer converted source after caller mutation: %s", got)
	}
}

func TestTimerPreservesBoundAndArrowReceivers(t *testing.T) {
	h, r, _ := testRealm(t, "https://timer.example.test/")
	evalTest(t, r, `var receivers=[], owner={fixture:true};
setTimeout(function(){"use strict"; receivers.push(this===owner)}.bind(owner), 0);
(function(){setTimeout(()=>receivers.push(this===owner), 0)}).call(owner);`)
	drainTasks(t, h)
	if got := evalTest(t, r, `JSON.stringify(receivers)`).String(); got != `[true,true]` {
		t.Fatalf("timer replaced an explicitly bound or lexical receiver: %s", got)
	}
}

func TestTimerUsesOriginalIntrinsics(t *testing.T) {
	h, r, _ := testRealm(t, "https://timer.example.test/")
	evalTest(t, r, `var originalGlobal=globalThis, callbackResult, sourceResult;
var unexpectedIntrinsic=()=>{throw new Error("replacement intrinsic called")};
Reflect.apply=String=eval=unexpectedIntrinsic;
globalThis={replacement:true};
setTimeout(function(value){"use strict"; callbackResult={global:this===originalGlobal,value}}, 0, "fixture");
setTimeout({toString(){return 'sourceResult={global:this===originalGlobal,scope:typeof callback}'}}, 0);`)
	drainTasks(t, h)
	got := evalTest(t, r, `JSON.stringify({callbackResult,sourceResult})`).String()
	if got != `{"callbackResult":{"global":true,"value":"fixture"},"sourceResult":{"global":true,"scope":"undefined"}}` {
		t.Fatalf("timer depended on mutable globals or leaked its closure to string source: %s", got)
	}
}

func TestTimerBelongsToItsRealm(t *testing.T) {
	h, parent, _ := testRealm(t, "https://parent.example.test/")
	child, err := h.newRealm(map[string]any{"url": "https://child.example.test/"}, parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*realm{parent, child} {
		evalTest(t, r, `var callbackGlobal, sourceGlobal;
setTimeout(function(){"use strict"; callbackGlobal=this}, 0);
setTimeout("sourceGlobal=this", 0);`)
	}
	drainTasks(t, h)
	for name, r := range map[string]*realm{"parent": parent, "child": child} {
		for _, result := range []string{"callbackGlobal", "sourceGlobal"} {
			if r.vm.Get(result) != r.vm.GlobalObject() {
				t.Errorf("%s timer %s received another realm's global", name, result)
			}
		}
	}
}

func TestIntervalSourceIsConvertedOnce(t *testing.T) {
	h, r, _ := testRealm(t, "https://timer.example.test/")
	evalTest(t, r, `var conversions=0, ticks=0, observed=[], sourceValue="initial";
var timerID=setInterval({toString(){
  conversions++;
  return 'observed.push('+JSON.stringify(sourceValue)+'); if(++ticks===2)clearInterval(timerID)';
}}, 0);
var conversionsBeforeCallback=conversions;
sourceValue="changed";`)
	if len(h.tasks) != 1 || h.tasks[0].repeat <= 0 {
		t.Fatal("repeating timer was not scheduled exactly once")
	}
	// The event loop keeps a repeat registered while invoking it, so the
	// second callback can remove that registration with clearInterval.
	task := h.tasks[0]
	for i := 0; i < 2; i++ {
		if len(h.tasks) != 1 {
			t.Fatal("timer ended before its second invocation")
		}
		if err := task.run(); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.tasks) != 0 {
		t.Fatal("interval source did not cancel its repeat")
	}
	got := evalTest(t, r, `JSON.stringify({observed,conversions,conversionsBeforeCallback})`).String()
	if got != `{"observed":["initial","initial"],"conversions":1,"conversionsBeforeCallback":1}` {
		t.Fatalf("interval reconverted its source between callbacks: %s", got)
	}
}

func TestTimerSourceConversionFailureDoesNotSchedule(t *testing.T) {
	h, r, _ := testRealm(t, "https://timer.example.test/")
	evalTest(t, r, `var sourceError=new Error("fixture"), conversionError;
try { setTimeout({toString(){throw sourceError}}, 0); }
catch (error) { conversionError=error; }`)
	if got := evalTest(t, r, `conversionError===sourceError`).ToBoolean(); !got {
		t.Fatal("timer source conversion error did not return to its caller")
	}
	if len(h.tasks) != 0 {
		t.Fatal("timer source conversion failure left scheduled work")
	}
}

func TestTimerCallbackFailureRemainsFatal(t *testing.T) {
	h, r, _ := testRealm(t, "https://timer.example.test/")
	evalTest(t, r, `setTimeout(function(){throw new Error("private fixture text")}, 0);`)
	if len(h.tasks) != 1 {
		t.Fatal("timer was not scheduled exactly once")
	}
	err := h.tasks[0].run()
	var diagnostic *Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != "SCRIPT_EXCEPTION" || diagnostic.Phase != "timer_callback" {
		t.Fatalf("timer failure lost its diagnostic: %v", err)
	}
	if diagnostic.Location == nil || !ValidDiagnosticLocation(diagnostic.Location) {
		t.Fatal("timer failure lost its numeric engine location")
	}
	want := fmt.Sprintf("GO_JS_RUNTIME: SCRIPT_EXCEPTION [phase=timer_callback] [line=%d column=%d]", diagnostic.Location.Line, diagnostic.Location.Column)
	if got := err.Error(); got != want {
		t.Fatalf("timer failure exposed script details or changed its public diagnostic: %s", got)
	}
}
