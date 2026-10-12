package main

import (
	"fmt"
	"time"

	"github.com/dop251/goja"
	"github.com/grafana/sobek"
)

// A direct-eval timer wrapper can corrupt later global lexical bindings even
// when the native timer only stores a callback and never invokes JavaScript.
func timerLexicalProbe(engine string, indirect bool) (r result) {
	start := time.Now()
	mode := "direct_eval"
	evaluation := `eval(String(fn))`
	if indirect {
		mode = "indirect_eval"
		evaluation = `(0,eval)(String(fn))`
	}
	r = result{Engine: engine, Case: "timer_global_lexical_" + mode}
	defer func() {
		if p := recover(); p != nil {
			r.Error = fmt.Sprintf("engine panic: %v", p)
		}
		r.Milliseconds = float64(time.Since(start).Microseconds()) / 1000
	}()
	bootstrap := `globalThis.setTimeout=(fn,ms=0,...args)=>__host.timeout(()=>typeof fn==="function"?fn(...args):` + evaluation + `,ms,false);globalThis.clearTimeout=id=>__host.clearTimer(Number(id));`
	const source = `let id=setTimeout(()=>42,0);clearTimeout(id);id;`
	var err error
	if engine == "sobek" {
		vm := sobek.New()
		host := vm.NewObject()
		_ = host.Set("timeout", func(sobek.FunctionCall) sobek.Value { return vm.ToValue(1) })
		_ = host.Set("clearTimer", func(int64) {})
		_ = vm.Set("__host", host)
		_, err = vm.RunString(bootstrap)
		if err == nil {
			var value sobek.Value
			value, err = vm.RunString(source)
			if err == nil {
				r.Actual = value.Export()
			}
		}
	} else {
		vm := goja.New()
		host := vm.NewObject()
		_ = host.Set("timeout", func(goja.FunctionCall) goja.Value { return vm.ToValue(1) })
		_ = host.Set("clearTimer", func(int64) {})
		_ = vm.Set("__host", host)
		_, err = vm.RunString(bootstrap)
		if err == nil {
			var value goja.Value
			value, err = vm.RunString(source)
			if err == nil {
				r.Actual = value.Export()
			}
		}
	}
	if err != nil {
		r.Error = err.Error()
	}
	r.Passed = r.Error == "" && fmt.Sprint(r.Actual) == "1"
	return
}
