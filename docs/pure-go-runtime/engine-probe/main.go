// This standalone module evaluates JavaScript locally, without Node, browser,
// account requests, dynamic SDK downloads or production dependencies.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/grafana/sobek"
)

type machine struct {
	run       func(string) (any, error)
	compile   func(string, string) error
	interrupt func(any)
	clear     func()
	promise   func() (func(any) error, error)
}

func newGoja() machine {
	r := goja.New()
	return machine{
		compile: func(name, source string) error { _, err := goja.Compile(name, source, false); return err },
		run: func(source string) (any, error) {
			v, err := r.RunString(source)
			if err != nil {
				return nil, err
			}
			return v.Export(), nil
		},
		interrupt: r.Interrupt, clear: r.ClearInterrupt,
		promise: func() (func(any) error, error) {
			p, resolve, _ := r.NewPromise()
			return resolve, r.Set("hostPromise", p)
		},
	}
}

func newSobek() machine {
	r := sobek.New()
	return machine{
		compile: func(name, source string) error { _, err := sobek.Compile(name, source, false); return err },
		run: func(source string) (any, error) {
			v, err := r.RunString(source)
			if err != nil {
				return nil, err
			}
			return v.Export(), nil
		},
		interrupt: r.Interrupt, clear: r.ClearInterrupt,
		promise: func() (func(any) error, error) {
			p, resolve, _ := r.NewPromise()
			return resolve, r.Set("hostPromise", p)
		},
	}
}

type result struct {
	Engine       string  `json:"engine"`
	Case         string  `json:"case"`
	Passed       bool    `json:"passed"`
	Actual       any     `json:"actual,omitempty"`
	Error        string  `json:"error,omitempty"`
	Milliseconds float64 `json:"milliseconds"`
}

type jsCase struct{ name, source, inspect, expected string }

var cases = []jsCase{
	{"promise_microtasks", `var trace=["sync"]; Promise.resolve().then(()=>{trace.push("first");Promise.resolve().then(()=>trace.push("inner"));});Promise.resolve().then(()=>trace.push("second"));trace.push("tail");`, `JSON.stringify(trace)`, `["sync","tail","first","second","inner"]`},
	{"async_await_rejection", `var trace=[];(async()=>{trace.push("entered");const n=await Promise.resolve(7);trace.push(n);try{await Promise.reject(new Error("rejected"));}catch(e){trace.push(e.message);}trace.push("done");})();trace.push("sync");`, `JSON.stringify(trace)`, `["entered","sync",7,"rejected","done"]`},
	{"promise_combinators", `var trace=[];(async()=>{const x=await Promise.allSettled([Promise.resolve(1),Promise.reject("no")]);const y=await Promise.any([Promise.reject("no"),Promise.resolve(2)]);trace.push(x[0].value,x[1].status,y);})();`, `JSON.stringify(trace)`, `[1,"rejected",2]`},
	{"class_private_fields", `class Base{constructor(x){this.x=x;} value(){return this.x;}}class Child extends Base{#secret=5;static #counter=2;static{this.#counter++;}value(){return super.value()+this.#secret;}static counter(){return this.#counter;}}var x=new Child(7);var answer=[x.value(),Child.counter(),x instanceof Base];`, `JSON.stringify(answer)`, `[12,3,true]`},
	{"optional_chaining_nullish", `var answer=[({a:{b:3}})?.a?.b,({a:null}).a?.b??4,0??5,null??6,({f(x){return x+1;}})?.f?.(8)];`, `JSON.stringify(answer)`, `[3,4,0,6,9]`},
	{"typed_arrays_dataview_bigint", `var bytes=new Uint8Array([1,2,3,4]);var view=new DataView(bytes.buffer);var nums=new BigInt64Array([1n,-2n]);var answer=[view.getUint32(0,true),Array.from(bytes.slice(1)),Array.from(nums,x=>String(x)),new Float64Array([1.5])[0]];`, `JSON.stringify(answer)`, `[67305985,[2,3,4],["1","-2"],1.5]`},
	{"regexp_named_lookbehind_unicode", `var match=/(?<=token-)(?<digits>\d+)/.exec("token-42");var answer=[match.groups.digits,/\p{Script=Han}+/u.test("中文"),/a+/gi.flags];`, `JSON.stringify(answer)`, `["42",true,"gi"]`},
	{"regexp_match_indices", `var match=/(foo)/d.exec("xfoo");var answer=match.indices;`, `JSON.stringify(answer)`, `[[1,4],[1,4]]`},
	{"regexp_unicode_sets", `var answer=/[\p{ASCII}&&\p{Letter}]/v.test("A");`, `JSON.stringify(answer)`, `true`},
	{"generator", `function* values(){yield 1;yield* [2,3];}var answer=[...values()];`, `JSON.stringify(answer)`, `[1,2,3]`},
	{"async_generator", `var answer=[];async function* values(){yield 1;yield await Promise.resolve(2);}(async()=>{for await(const n of values()){answer.push(n);}})();`, `JSON.stringify(answer)`, `[1,2]`},
	{"eval_dynamic_function", `var answer=[eval("({a:4})").a,(new Function("x","return x?.a ?? 5"))({a:6})];`, `JSON.stringify(answer)`, `[4,6]`},
	{"host_api_inventory", `var answer=[typeof URL,typeof TextEncoder,typeof crypto,typeof document,typeof queueMicrotask,typeof setTimeout];`, `JSON.stringify(answer)`, `["undefined","undefined","undefined","undefined","undefined","undefined"]`},
}

func evaluate(name string, factory func() machine, c jsCase) result {
	start := time.Now()
	m := factory()
	r := result{Engine: name, Case: c.name}
	_, err := m.run(c.source)
	if err == nil {
		r.Actual, err = m.run(c.inspect)
	}
	if err != nil {
		r.Error = err.Error()
	} else {
		r.Passed = fmt.Sprint(r.Actual) == c.expected
	}
	r.Milliseconds = float64(time.Since(start).Microseconds()) / 1000
	return r
}

func hostPromise(name string, factory func() machine) result {
	start := time.Now()
	m := factory()
	r := result{Engine: name, Case: "host_resolved_promise"}
	resolve, err := m.promise()
	if err == nil {
		_, err = m.run(`var answer="pending";(async()=>{answer=await hostPromise;})();`)
	}
	if err == nil {
		var pending any
		pending, err = m.run(`answer`)
		if err == nil && pending != "pending" {
			err = fmt.Errorf("promise resolved before host response: %v", pending)
		}
	}
	if err == nil {
		err = resolve("host response")
	}
	if err == nil {
		r.Actual, err = m.run(`answer`)
	}
	if err != nil {
		r.Error = err.Error()
	} else {
		r.Passed = r.Actual == "host response"
	}
	r.Milliseconds = float64(time.Since(start).Microseconds()) / 1000
	return r
}

func interrupt(name string, factory func() machine, caseName, source string) result {
	start := time.Now()
	m := factory()
	r := result{Engine: name, Case: caseName}
	done := make(chan error, 1)
	go func() { _, err := m.run(source); done <- err }()
	timer := time.AfterFunc(20*time.Millisecond, func() { m.interrupt("probe deadline") })
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "probe deadline") {
			r.Error = fmt.Sprintf("expected interruption: %v", err)
			break
		}
		m.clear()
		v, err := m.run(`6*7`)
		r.Actual = v
		r.Passed = err == nil && fmt.Sprint(v) == "42"
		if err != nil {
			r.Error = err.Error()
		}
	case <-time.After(2 * time.Second):
		r.Error = "VM did not observe interruption within 2 seconds"
	}
	r.Milliseconds = float64(time.Since(start).Microseconds()) / 1000
	return r
}

func isolate(name string, factory func() machine) result {
	start := time.Now()
	r := result{Engine: name, Case: "parallel_runtime_isolation"}
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, b := factory(), factory()
			_, err := a.run(fmt.Sprintf(`globalThis.secret="private-%d";Array.prototype.privateProbe=7;`, i))
			if err == nil {
				var v any
				v, err = b.run(`JSON.stringify([typeof secret,typeof Array.prototype.privateProbe])`)
				if err == nil && v != `["undefined","undefined"]` {
					err = fmt.Errorf("cross-runtime state leak: %v", v)
				}
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	r.Passed = true
	for err := range errs {
		if err != nil {
			r.Passed = false
			r.Error = err.Error()
		}
	}
	r.Actual = "8 independently concurrent pairs"
	r.Milliseconds = float64(time.Since(start).Microseconds()) / 1000
	return r
}

func parsePublicSDK(name string, factory func() machine, sdkDir string) []result {
	var out []result
	for _, file := range []string{"loader.js", "sdk.js"} {
		start := time.Now()
		r := result{Engine: name, Case: "compile_public_" + file}
		source, err := os.ReadFile(sdkDir + "/" + file)
		if err == nil {
			err = factory().compile(file, string(source))
		}
		if err != nil {
			r.Error = err.Error()
		} else {
			r.Passed = true
			r.Actual = fmt.Sprintf("compiled %d bytes; not executed", len(source))
		}
		r.Milliseconds = float64(time.Since(start).Microseconds()) / 1000
		out = append(out, r)
	}
	return out
}

func crossRuntimeObject(name string) (r result) {
	start := time.Now()
	r = result{Engine: name, Case: "cross_runtime_object_transfer"}
	defer func() {
		if p := recover(); p != nil {
			r.Error = fmt.Sprint(p)
		}
		r.Milliseconds = float64(time.Since(start).Microseconds()) / 1000
	}()
	const source = `globalThis.secret="realmA";({f(){return globalThis.secret},nodeType:1})`
	const inspect = `globalThis.secret="realmB";JSON.stringify([foreign.f(),foreign===foreign,Object.getPrototypeOf(foreign)===Object.prototype])`
	if name == "sobek" {
		a, b := sobek.New(), sobek.New()
		v, err := a.RunString(source)
		if err == nil {
			err = b.Set("foreign", v)
		}
		if err == nil {
			var answer sobek.Value
			answer, err = b.RunString(inspect)
			if err == nil {
				r.Actual = answer.Export()
			}
		}
		if err != nil {
			r.Error = err.Error()
		}
	} else {
		a, b := goja.New(), goja.New()
		v, err := a.RunString(source)
		if err == nil {
			err = b.Set("foreign", v)
		}
		if err == nil {
			var answer goja.Value
			answer, err = b.RunString(inspect)
			if err == nil {
				r.Actual = answer.Export()
			}
		}
		if err != nil {
			r.Error = err.Error()
		}
	}
	r.Passed = r.Error == "" && r.Actual == `["realmA",true,false]`
	return
}

func main() {
	sdkDir := ""
	if len(os.Args) > 1 {
		sdkDir = os.Args[1]
	}
	var results []result
	for _, engine := range []struct {
		name string
		new  func() machine
	}{{"goja", newGoja}, {"sobek", newSobek}} {
		for _, c := range cases {
			results = append(results, evaluate(engine.name, engine.new, c))
		}
		results = append(results, hostPromise(engine.name, engine.new), interrupt(engine.name, engine.new, "interrupt_and_reuse", `for(;;){}`), interrupt(engine.name, engine.new, "interrupt_microtask_loop_and_reuse", `Promise.resolve().then(function loop(){return Promise.resolve().then(loop);});`), isolate(engine.name, engine.new))
		results = append(results, crossRuntimeObject(engine.name))
		results = append(results, timerLexicalProbe(engine.name, false), timerLexicalProbe(engine.name, true))
		if sdkDir != "" {
			results = append(results, parsePublicSDK(engine.name, engine.new, sdkDir)...)
		}
	}
	versions := map[string]string{}
	settings := map[string]string{}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			versions[dep.Path] = dep.Version
		}
		for _, setting := range info.Settings {
			settings[setting.Key] = setting.Value
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(map[string]any{"go_version": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH, "build_settings": settings, "modules": versions, "results": results}); err != nil {
		panic(err)
	}
}
