package jshost

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/sobek"
	whatwg "github.com/nlnwa/whatwg-url/url"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

type command map[string]any
type inputEvent struct {
	command command
	err     error
}
type task struct {
	id     int64
	at     time.Time
	realm  *realm
	run    func() error
	repeat time.Duration
}
type host struct {
	ctx         context.Context
	kind        string
	input       map[string]any
	writer      *json.Encoder
	commands    <-chan inputEvent
	deferred    []inputEvent
	realms      []*realm
	root        *realm
	activeFrame *realm
	tasks       []*task
	pending     map[int64]func(command) error
	sequence    int64
	errorCount  int
	started     time.Time
	finished    bool
	terminalErr error
	async       chan func() error
	workers     map[int64]*webWorker
	allWorkers  []*webWorker
	blobs       map[string]blobResource
	storage     storageAreas
}
type realm struct {
	host          *host
	vm            *sobek.Runtime
	config        map[string]any
	url           string
	parent        *realm
	top           *realm
	stopInterrupt func() bool
	ready         bool
	frames        map[*sobek.Object]*realm
	loaded        map[*sobek.Object]string
	windows       map[*windowRef]*sobek.Object
	wrappers      map[foreignRef]sobek.Value
	foreign       map[*sobek.Object]foreignRef
	unhandled     map[*sobek.Promise]bool
	window        *windowRef
	retired       bool
	scriptEntry   sobek.Callable
}

// Run owns every engine instance on this goroutine. Input decoding and context
// interruption never execute JavaScript; all callbacks run through this loop.
func Run(ctx context.Context, kind string, inputJSON json.RawMessage, commands io.Reader, events io.Writer) error {
	input, err := parseInput(inputJSON)
	if err != nil {
		return err
	}
	if kind != "sentinel" && kind != "parent" && kind != "child" && kind != "webworker" {
		return &Error{Code: "KIND_INVALID"}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	incoming := make(chan inputEvent, 32)
	if closer, ok := commands.(io.Closer); ok {
		defer closer.Close()
	}
	go func() {
		scanner := bufio.NewScanner(commands)
		scanner.Buffer(make([]byte, 65536), maxMessageBytes)
		for scanner.Scan() {
			var value command
			if json.Unmarshal(scanner.Bytes(), &value) != nil || value == nil {
				select {
				case incoming <- inputEvent{err: &Error{Code: "PROTOCOL_INVALID"}}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case incoming <- inputEvent{command: value}:
			case <-ctx.Done():
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		select {
		case incoming <- inputEvent{err: err}:
		case <-ctx.Done():
		}
	}()
	h := &host{ctx: ctx, kind: kind, input: input, writer: json.NewEncoder(events), commands: incoming, pending: map[int64]func(command) error{}, started: time.Now()}
	defer func() {
		for _, worker := range h.allWorkers {
			worker.stop()
		}
		for _, worker := range h.allWorkers {
			<-worker.done
			<-worker.forwardDone
			<-worker.writerDone
		}
		for _, r := range h.realms {
			r.stopInterrupt()
			r.vm.Interrupt("HOST_CLOSED")
		}
		h.tasks = nil
		h.pending = nil
	}()
	if err = h.initialize(); err != nil {
		return err
	}
	for !h.finished {
		if err = ctx.Err(); err != nil {
			return err
		}
		for _, r := range h.realms {
			if !r.retired && len(r.unhandled) > 0 {
				return &Error{Code: "SCRIPT_UNHANDLED_REJECTION"}
			}
		}
		if len(h.deferred) > 0 {
			e := h.deferred[0]
			h.deferred = h.deferred[1:]
			if err = h.receive(e); err != nil {
				return err
			}
			continue
		}
		// Service one ready transport/worker event before the next timer task.
		// A script that continually queues zero-delay timers must not starve
		// its network responses or another worker's messages.
		select {
		case e := <-incoming:
			if err = h.receive(e); err != nil {
				return err
			}
		case fn := <-h.async:
			if err = fn(); err != nil {
				return h.scriptError(err)
			}
		default:
		}
		if h.finished {
			break
		}
		index := -1
		for i, t := range h.tasks {
			if index < 0 || t.at.Before(h.tasks[index].at) || t.at.Equal(h.tasks[index].at) && t.id < h.tasks[index].id {
				index = i
			}
		}
		wait := time.Hour
		if index >= 0 {
			wait = time.Until(h.tasks[index].at)
		}
		if wait <= 0 {
			t := h.tasks[index]
			h.tasks = append(h.tasks[:index], h.tasks[index+1:]...)
			if t.repeat > 0 {
				t.at = time.Now().Add(t.repeat)
				h.tasks = append(h.tasks, t)
			}
			if err = t.run(); err != nil {
				return h.scriptError(err)
			}
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case e := <-incoming:
			timer.Stop()
			if err = h.receive(e); err != nil {
				return err
			}
		case fn := <-h.async:
			timer.Stop()
			if err = fn(); err != nil {
				return h.scriptError(err)
			}
		case <-timer.C:
		}
	}
	return h.terminalErr
}

func (h *host) emit(value any) error {
	if err := h.writer.Encode(value); err != nil {
		return &Error{Code: "OUTPUT_CLOSED"}
	}
	return nil
}
func (h *host) nextID() int64 { h.sequence++; return h.sequence }
func (h *host) schedule(r *realm, delay time.Duration, repeat time.Duration, fn func() error) int64 {
	if r.retired {
		return 0
	}
	if len(h.tasks) >= 4096 {
		panic(r.vm.NewTypeError("JS_RESOURCE_LIMIT_TIMERS"))
	}
	id := h.nextID()
	h.tasks = append(h.tasks, &task{id: id, at: time.Now().Add(max(delay, 0)), realm: r, run: fn, repeat: repeat})
	return id
}
func (h *host) clearTimer(id int64) {
	for i, t := range h.tasks {
		if t.id == id {
			h.tasks = append(h.tasks[:i], h.tasks[i+1:]...)
			return
		}
	}
}

func (r *realm) retire() {
	if r.retired {
		return
	}
	r.retired = true
	for _, child := range r.frames {
		child.retire()
	}
	filtered := r.host.tasks[:0]
	for _, task := range r.host.tasks {
		if task.realm != r {
			filtered = append(filtered, task)
		}
	}
	r.host.tasks = filtered
	for id, worker := range r.host.workers {
		if worker.owner == r {
			worker.stop()
			delete(r.host.workers, id)
		}
	}
}
func (h *host) scriptError(err error) error {
	if h.ctx.Err() != nil {
		return h.ctx.Err()
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	// Classify known host errors, never forward remote exception text.
	for _, code := range hostErrorCodes {
		if strings.Contains(err.Error(), code) {
			return &Error{Code: code}
		}
	}
	code := "SCRIPT_EXCEPTION"
	if exception, ok := err.(*sobek.Exception); ok {
		for _, name := range []string{"ReferenceError", "TypeError", "SyntaxError", "RangeError"} {
			if strings.HasPrefix(exception.String(), name+":") {
				code = "SCRIPT_" + strings.ToUpper(name)
				break
			}
		}
	}
	return &Error{Code: code}
}

var hostErrorCodes = []string{"JS_API_UNSUPPORTED_WORKER", "JS_API_UNSUPPORTED_MODULE_WORKER", "JS_API_UNSUPPORTED_MODULE_SCRIPT", "JS_API_UNSUPPORTED_WEBASSEMBLY", "JS_API_UNSUPPORTED_SYNCHRONOUS_XHR", "JS_API_UNSUPPORTED_XHR_RESPONSE", "JS_RESOURCE_LIMIT_TIMERS", "JS_RESOURCE_LIMIT_REALMS", "JS_RESOURCE_LIMIT_REQUESTS", "JS_RESOURCE_LIMIT_BLOBS", "JS_RESOURCE_LIMIT_WORKERS", "JS_RESOURCE_LIMIT_WORKER_MESSAGES", "JS_RESOURCE_LIMIT_STORAGE"}

func (h *host) receive(e inputEvent) error {
	if e.err != nil {
		if e.err == io.EOF {
			h.finished = true
			return nil
		}
		if diagnostic, ok := e.err.(*Error); ok {
			return diagnostic
		}
		return &Error{Code: "INPUT_CLOSED"}
	}
	c := e.command
	switch stringField(c, "type") {
	case "worker-message":
		if h.kind != "webworker" {
			return &Error{Code: "MESSAGE_TARGET_INVALID"}
		}
		return h.queueMessage(h.root, nil, copiedMessage{graph: c["graph"]}, "")
	case "shutdown":
		h.finished = true
		return nil
	case "call":
		method, flow := stringField(c, "method"), stringField(c, "flow")
		if h.kind != "sentinel" || method != "token" && method != "sessionObserverToken" {
			return &Error{Code: "CALL_INVALID"}
		}
		return h.callSDK(method, flow, time.Now().Add(15*time.Second))
	case "parent-message":
		if h.kind != "child" {
			return &Error{Code: "MESSAGE_TARGET_INVALID"}
		}
		return h.queueMessage(h.root, nil, commandMessageData(c), stringField(h.input, "parentUrl"))
	case "child-message":
		if h.kind != "parent" || h.activeFrame == nil {
			return &Error{Code: "MESSAGE_SOURCE_INVALID"}
		}
		return h.queueMessage(h.root, h.activeFrame, commandMessageData(c), h.activeFrame.url)
	default:
		id := numberID(c["id"])
		if resolve := h.pending[id]; resolve != nil {
			delete(h.pending, id)
			return resolve(c)
		}
		return &Error{Code: "REPLY_UNKNOWN"}
	}
}
func numberID(value any) int64 {
	if n, ok := value.(float64); ok {
		return int64(n)
	}
	if n, ok := value.(int64); ok {
		return n
	}
	return 0
}

func (h *host) initialize() error {
	r, err := h.newRealm(h.input, nil)
	if err != nil {
		return err
	}
	h.root = r
	if h.kind == "webworker" {
		source, ok := h.input["workerSource"].(string)
		if !ok {
			source, err = r.workerSource(r.url)
			if err != nil {
				return err
			}
		}
		_, err = r.vm.RunScript("worker-script.js", source)
		if err != nil {
			return h.scriptError(err)
		}
		return nil
	}
	if h.kind == "sentinel" {
		loader := r.vm.Get("document").ToObject(r.vm)
		create, _ := sobek.AssertFunction(loader.Get("createElement"))
		v, err := create(loader, r.vm.ToValue("script"))
		if err != nil {
			return h.scriptError(err)
		}
		script := v.ToObject(r.vm)
		_ = script.Set("src", stringField(h.input, "loaderUrl"))
		// Include the loader in document.scripts without requesting it twice.
		r.loaded[script] = stringField(h.input, "loaderUrl")
		head := loader.Get("head").ToObject(r.vm)
		appendChild, _ := sobek.AssertFunction(head.Get("appendChild"))
		if _, err = appendChild(head, script); err != nil {
			return h.scriptError(err)
		}
		if err = r.runDocumentScript(script, "sentinel-loader.js", stringField(h.input, "loaderSource")); err != nil {
			return h.scriptError(err)
		}
		return nil
	}
	if h.kind == "child" {
		// The parent is an external context, represented by one stable proxy.
		parentConfig := map[string]any{"url": stringField(h.input, "parentUrl"), "html": "<html><body></body></html>"}
		parent, err := h.newRealm(parentConfig, nil)
		if err != nil {
			return err
		}
		r.parent = parent
		r.top = parent
		_ = r.vm.Set("parent", r.windowProxy(parent))
		_ = r.vm.Set("top", r.windowProxy(parent))
		// queueMessage already defers delivery until the event loop runs. Queue
		// the initial messages now so live stdin commands cannot overtake them.
		if values, ok := h.input["parentMessages"].([]any); ok {
			graphs, _ := h.input["parentMessageGraphs"].([]any)
			for index, data := range values {
				if index < len(graphs) && graphs[index] != nil {
					data = copiedMessage{graph: graphs[index]}
				}
				if err := h.queueMessage(r, nil, data, parent.url); err != nil {
					return err
				}
			}
		}
	}
	if err = r.runInitialScripts(); err != nil {
		return h.scriptError(err)
	}
	_, err = r.vm.RunString(`document.readyState="complete"; document.dispatchEvent(new Event("DOMContentLoaded",{bubbles:true})); window.dispatchEvent(new Event("load"));`)
	if err != nil {
		return h.scriptError(err)
	}
	wait := numberID(h.input["waitMs"])
	if wait <= 0 {
		wait = 10000
	}
	h.schedule(r, time.Duration(wait)*time.Millisecond, 0, func() error {
		h.finished = true
		return h.emit(map[string]any{"type": "final", "result": map[string]any{"errors": []any{}, "reportedErrors": []any{}}})
	})
	return nil
}

func (h *host) newRealm(config map[string]any, parent *realm) (*realm, error) {
	if len(h.realms) >= 32 {
		return nil, &Error{Code: "JS_RESOURCE_LIMIT_REALMS"}
	}
	vm := sobek.New()
	vm.SetMaxCallStackSize(2048)
	r := &realm{host: h, vm: vm, config: config, url: stringField(config, "url"), parent: parent, frames: map[*sobek.Object]*realm{}, loaded: map[*sobek.Object]string{}, windows: map[*windowRef]*sobek.Object{}, wrappers: map[foreignRef]sobek.Value{}, foreign: map[*sobek.Object]foreignRef{}}
	entry, err := vm.RunString(`(run => run())`)
	if err != nil {
		return nil, h.scriptError(err)
	}
	r.scriptEntry, _ = sobek.AssertFunction(entry)
	r.window = &windowRef{realm: r}
	r.unhandled = map[*sobek.Promise]bool{}
	vm.SetPromiseRejectionTracker(func(p *sobek.Promise, operation sobek.PromiseRejectionOperation) {
		if operation == sobek.PromiseRejectionReject {
			r.unhandled[p] = true
		} else {
			delete(r.unhandled, p)
		}
	})
	r.top = r
	if parent != nil {
		r.top = parent.top
	}
	r.stopInterrupt = context.AfterFunc(h.ctx, func() { vm.Interrupt("CONTEXT_CANCELED") })
	h.realms = append(h.realms, r)
	_ = vm.Set("__config", config)
	_ = vm.Set("window", vm.GlobalObject())
	_ = vm.Set("self", vm.GlobalObject())
	_ = vm.Set("__sentinelFrame", h.kind == "sentinel" && parent != nil)
	_ = vm.Set("global", sobek.Undefined())
	if err := r.installBridges(); err != nil {
		return nil, err
	}
	if err := installWebPlatform(vm); err != nil {
		return nil, h.scriptError(err)
	}
	if _, err := vm.RunString(`globalThis.location=new URL(__config.url);`); err != nil {
		return nil, h.scriptError(err)
	}
	if parent == nil {
		_ = vm.Set("parent", vm.GlobalObject())
		_ = vm.Set("top", vm.GlobalObject())
	} else {
		_ = vm.Set("parent", r.windowProxy(parent))
		_ = vm.Set("top", r.windowProxy(r.top))
	}
	if _, err := vm.RunScript("browser-dom.js", domSource); err != nil {
		return nil, h.scriptError(err)
	}
	if _, err := vm.RunScript("browser-network.js", networkSource); err != nil {
		return nil, h.scriptError(err)
	}
	if _, err := vm.RunScript("structured-clone.js", cloneSource); err != nil {
		return nil, h.scriptError(err)
	}
	if err := installStorage(vm, &h.storage, origin(r.url)); err != nil {
		return nil, h.scriptError(err)
	}
	if _, err := vm.RunScript("web-workers.js", workerSource); err != nil {
		return nil, h.scriptError(err)
	}
	r.ready = true
	return r, nil
}

func (r *realm) installBridges() error {
	h, vm := r.host, r.vm
	native := vm.NewObject()
	_ = native.Set("parseHTML", func(call sobek.FunctionCall) sobek.Value {
		contextTag := ""
		if len(call.Arguments) > 1 && !sobek.IsUndefined(call.Argument(1)) {
			contextTag = call.Argument(1).String()
		}
		return vm.ToValue(parseHTML(call.Argument(0).String(), contextTag))
	})
	_ = native.Set("inserted", func(call sobek.FunctionCall) sobek.Value {
		if r.ready {
			if err := r.inserted(call.Argument(0).ToObject(vm)); err != nil {
				panic(vm.NewTypeError(h.scriptError(err).Error()))
			}
		}
		return sobek.Undefined()
	})
	_ = native.Set("frame", func(call sobek.FunctionCall) sobek.Value {
		node := call.Argument(0).ToObject(vm)
		child, err := r.frame(node)
		if err != nil {
			panic(vm.NewTypeError(h.scriptError(err).Error()))
		}
		return r.windowProxy(child)
	})
	_ = native.Set("removed", func(call sobek.FunctionCall) sobek.Value {
		if !r.ready {
			return sobek.Undefined()
		}
		node := call.Argument(0).ToObject(vm)
		for element, child := range r.frames {
			contains, _ := sobek.AssertFunction(node.Get("contains"))
			contained, err := contains(node, element)
			if err == nil && contained.ToBoolean() {
				child.retire()
				delete(r.frames, element)
				delete(r.loaded, element)
			}
		}
		return sobek.Undefined()
	})
	_ = native.Set("cookieGet", func() string { return r.cookie("get", "") })
	_ = native.Set("cookieSet", func(value string) { r.cookie("set", value) })
	_ = native.Set("resolveURL", func(value, base string) string {
		u, err := url.Parse(value)
		b, e := url.Parse(base)
		if err != nil || e != nil {
			panic(vm.NewTypeError("Invalid URL"))
		}
		return b.ResolveReference(u).String()
	})
	_ = native.Set("request", func(call sobek.FunctionCall) sobek.Value { return r.request(call.Argument(0)) })
	_ = native.Set("result", func(method, flow string, value sobek.Value) {
		if h.finished {
			return
		}
		var result any
		if !sobek.IsNull(value) && !sobek.IsUndefined(value) {
			result = value.String()
		}
		if err := h.emit(map[string]any{"type": "result", "method": method, "flow": flow, "value": result, "errors": []any{}}); err != nil {
			h.terminalErr = err
			h.finished = true
		}
	})
	_ = native.Set("failure", func(call sobek.FunctionCall) sobek.Value {
		if h.finished {
			return sobek.Undefined()
		}
		code := "SCRIPT_REJECTION"
		if object, ok := call.Argument(0).(*sobek.Object); ok {
			name := object.Get("name")
			if name != nil && name.String() == "TypeError" {
				code = "SCRIPT_TYPEERROR"
			}
		}
		// Return the failure to the process entry point so its exit status and
		// single diagnostic event agree. Web Workers use the same Run result.
		h.terminalErr = &Error{Code: code}
		h.finished = true
		return sobek.Undefined()
	})
	_ = native.Set("now", func() float64 { return float64(time.Since(h.started).Nanoseconds()) / 1e6 })
	_ = native.Set("timeout", func(call sobek.FunctionCall) sobek.Value {
		fn, ok := sobek.AssertFunction(call.Argument(0))
		if !ok {
			panic(vm.NewTypeError("Timer callback must be callable"))
		}
		delay := time.Duration(min(max(call.Argument(1).ToInteger(), 0), 2147483647)) * time.Millisecond
		repeat := time.Duration(0)
		if call.Argument(2).ToBoolean() {
			repeat = max(delay, time.Millisecond)
		}
		return vm.ToValue(h.schedule(r, delay, repeat, func() error { _, err := fn(vm.GlobalObject()); return err }))
	})
	_ = native.Set("clearTimer", func(id int64) { h.clearTimer(id) })
	_ = native.Set("detachBuffer", func(call sobek.FunctionCall) sobek.Value {
		buffer, ok := call.Argument(0).Export().(sobek.ArrayBuffer)
		if !ok || buffer.Detached() {
			panic(vm.NewTypeError("DataCloneError"))
		}
		buffer.Detach()
		return sobek.Undefined()
	})
	_ = native.Set("postMessage", func(call sobek.FunctionCall) sobek.Value {
		data := r.messageCopyTransfer(call.Argument(0), call.Argument(2))
		if h.kind == "webworker" {
			if err := h.emit(map[string]any{"type": "worker-message", "graph": data.graph}); err != nil {
				panic(vm.NewTypeError("Worker message channel closed"))
			}
			return sobek.Undefined()
		}
		targetOrigin := call.Argument(1).String()
		if targetOrigin != "*" && targetOrigin != "/" && targetOrigin != origin(r.url) {
			return sobek.Undefined()
		}
		_ = h.queueMessage(r, r, data, r.url)
		return sobek.Undefined()
	})
	r.installWorkers(native)
	_ = vm.Set("__host", native)
	_, err := vm.RunString(`globalThis.setTimeout=(fn,ms=0,...args)=>__host.timeout(()=>typeof fn==="function"?fn(...args):(0,eval)(String(fn)),ms,false);globalThis.setInterval=(fn,ms=0,...args)=>__host.timeout(()=>typeof fn==="function"?fn(...args):(0,eval)(String(fn)),ms,true);globalThis.clearTimeout=globalThis.clearInterval=id=>__host.clearTimer(Number(id));globalThis.queueMicrotask=fn=>{if(typeof fn!=="function")throw new TypeError("callback must be a function");Promise.resolve().then(fn).catch(__host.failure);};`)
	return err
}

func (r *realm) clone(value sobek.Value) any {
	fn, _ := sobek.AssertFunction(r.vm.Get("JSON").ToObject(r.vm).Get("stringify"))
	v, err := fn(sobek.Undefined(), value)
	if err != nil {
		panic(r.vm.NewTypeError("DataCloneError"))
	}
	if sobek.IsUndefined(v) {
		return nil
	}
	var result any
	if json.Unmarshal([]byte(v.String()), &result) != nil {
		panic(r.vm.NewTypeError("DataCloneError"))
	}
	return result
}

type copiedMessage struct{ graph any }

func commandMessageData(c command) any {
	if graph := c["graph"]; graph != nil {
		return copiedMessage{graph: graph}
	}
	return c["data"]
}

func (r *realm) messageCopyTransfer(value, transfer sobek.Value) copiedMessage {
	var buffers []sobek.ArrayBuffer
	if transfer != nil && !sobek.IsUndefined(transfer) {
		from, _ := sobek.AssertFunction(r.vm.Get("Array").ToObject(r.vm).Get("from"))
		list, err := from(sobek.Undefined(), transfer)
		if err != nil {
			panic(r.vm.NewTypeError("DataCloneError"))
		}
		array := list.ToObject(r.vm)
		seen := map[sobek.ArrayBuffer]bool{}
		for i := int64(0); i < array.Get("length").ToInteger(); i++ {
			buffer, ok := array.Get(strconv.FormatInt(i, 10)).Export().(sobek.ArrayBuffer)
			if !ok || buffer.Detached() || seen[buffer] {
				panic(r.vm.NewTypeError("DataCloneError"))
			}
			seen[buffer] = true
			buffers = append(buffers, buffer)
		}
	}
	copy := r.messageCopy(value)
	for _, buffer := range buffers {
		buffer.Detach()
	}
	return copy
}

func (r *realm) messagePlain(copy copiedMessage) any {
	fn, _ := sobek.AssertFunction(r.vm.Get("__clonePlain"))
	value, err := fn(sobek.Undefined(), r.vm.ToValue(copy.graph))
	if err != nil {
		return nil
	}
	return r.clone(value)
}

func (r *realm) messageCopy(value sobek.Value) copiedMessage {
	fn, _ := sobek.AssertFunction(r.vm.Get("__clonePack"))
	packed, err := fn(sobek.Undefined(), value)
	if err != nil {
		panic(r.vm.NewTypeError("DataCloneError"))
	}
	return copiedMessage{graph: r.clone(packed)}
}
func (r *realm) cookie(action, value string) string {
	h := r.host
	id := h.nextID()
	if err := h.emit(map[string]any{"type": "cookie", "id": id, "url": r.url, "action": action, "value": value}); err != nil {
		panic(r.vm.NewTypeError("Cookie bridge closed"))
	}
	for {
		select {
		case <-h.ctx.Done():
			panic(r.vm.NewTypeError("Cookie bridge canceled"))
		case event := <-h.commands:
			if event.err != nil {
				panic(r.vm.NewTypeError("Cookie bridge closed"))
			}
			if stringField(event.command, "type") == "cookie-result" && numberID(event.command["id"]) == id {
				return stringField(event.command, "value")
			}
			h.deferred = append(h.deferred, event)
		}
	}
}
func (r *realm) request(value sobek.Value) sobek.Value {
	h := r.host
	if len(h.pending) >= 4096 {
		panic(r.vm.NewTypeError("JS_RESOURCE_LIMIT_REQUESTS"))
	}
	data, ok := r.clone(value).(map[string]any)
	if !ok {
		panic(r.vm.NewTypeError("Invalid request"))
	}
	id := h.nextID()
	data["id"] = id
	data["sourceUrl"] = r.url
	if networkURL := stringField(r.config, "networkURL"); networkURL != "" {
		data["sourceUrl"] = networkURL
	}
	promise, resolve, reject := r.vm.NewPromise()
	h.pending[id] = func(reply command) error {
		h.schedule(r, 0, 0, func() error {
			if code := stringField(reply, "error"); code != "" {
				return reject(r.vm.NewTypeError("Network request failed"))
			}
			return resolve(r.vm.ToValue(map[string]any(reply)))
		})
		return nil
	}
	if err := h.emit(map[string]any{"type": "request", "request": data}); err != nil {
		delete(h.pending, id)
		_ = reject(r.vm.NewTypeError("Network bridge closed"))
	}
	return r.vm.ToValue(promise)
}

func (h *host) callSDK(method, flow string, deadline time.Time) error {
	r := h.root
	sdk := r.vm.Get("SentinelSDK")
	if sdk == nil || sobek.IsUndefined(sdk) || sobek.IsNull(sdk) {
		if time.Now().After(deadline) {
			return &Error{Code: "SDK_INITIALIZATION_TIMEOUT"}
		}
		h.schedule(r, 10*time.Millisecond, 0, func() error { return h.callSDK(method, flow, deadline) })
		return nil
	}
	fn, ok := sobek.AssertFunction(r.vm.Get("__callSDK"))
	if !ok {
		return &Error{Code: "HOST_INITIALIZATION_FAILED"}
	}
	_, err := fn(sobek.Undefined(), r.vm.ToValue(method), r.vm.ToValue(flow))
	if err != nil {
		return h.scriptError(err)
	}
	return nil
}
func (h *host) queueMessage(target, source *realm, data any, sourceURL string) error {
	h.schedule(target, 0, 0, func() error {
		var sourceObject sobek.Value
		if source != nil {
			sourceObject = target.windowProxy(source)
		} else if target.parent != nil {
			sourceObject = target.windowProxy(target.parent)
		} else {
			sourceObject = sobek.Null()
		}
		fn, ok := sobek.AssertFunction(target.vm.Get("__dispatchMessage"))
		if !ok {
			return &Error{Code: "HOST_MESSAGE_UNAVAILABLE"}
		}
		value := target.vm.ToValue(data)
		if copied, ok := data.(copiedMessage); ok {
			unpack, _ := sobek.AssertFunction(target.vm.Get("__cloneUnpack"))
			var err error
			value, err = unpack(sobek.Undefined(), target.vm.ToValue(copied.graph))
			if err != nil {
				return err
			}
		}
		_, err := fn(sobek.Undefined(), value, sourceObject, target.vm.ToValue(origin(sourceURL)))
		return err
	})
	return nil
}
func origin(raw string) string {
	u, err := whatwg.Parse(raw)
	if err != nil {
		return "null"
	}
	return webURLParts(u)["origin"]
}

func (r *realm) frame(node *sobek.Object) (*realm, error) {
	raw := node.Get("src").String()
	if raw == "" {
		raw = r.url
	}
	previous := r.frames[node]
	if previous != nil && previous.url == raw {
		return previous, nil
	}
	config := map[string]any{}
	for k, v := range r.config {
		config[k] = v
	}
	config["html"] = "<!doctype html><html><head></head><body></body></html>"
	config["url"] = raw
	child, err := r.host.newRealm(config, r)
	if err != nil {
		return nil, err
	}
	if previous != nil {
		previous.retire()
		child.window = previous.window
		child.window.realm = child
	}
	r.frames[node] = child
	if origin(child.url) == origin(r.url) {
		_ = child.vm.Set("frameElement", child.importValue(r, node))
	} else {
		_ = child.vm.Set("frameElement", sobek.Null())
	}
	return child, nil
}
func (r *realm) inserted(node *sobek.Object) error {
	localName := node.Get("localName")
	if localName == nil || sobek.IsUndefined(localName) || sobek.IsNull(localName) {
		return nil
	}
	name := strings.ToLower(localName.String())
	if name != "script" && name != "iframe" {
		return nil
	}
	src := node.Get("src").String()
	if name == "script" {
		executable, err := r.executableScript(node)
		if err != nil || !executable {
			return err
		}
		if src == "" {
			if _, started := r.loaded[node]; started || node.Get("textContent").String() == "" {
				return nil
			}
			r.loaded[node] = "inline"
			return r.runDocumentScript(node, "dynamic-inline.js", node.Get("textContent").String())
		}
	}
	if src == "" || r.loaded[node] == src {
		return nil
	}
	r.loaded[node] = src
	h := r.host
	if name == "iframe" {
		child, err := r.frame(node)
		if err != nil {
			return err
		}
		if h.kind == "sentinel" && strings.Contains(src, "/backend-api/sentinel/frame.html") {
			document := child.vm.Get("document").ToObject(child.vm)
			create, _ := sobek.AssertFunction(document.Get("createElement"))
			script, err := create(document, child.vm.ToValue("script"))
			if err != nil {
				return err
			}
			_ = script.ToObject(child.vm).Set("src", stringField(h.input, "sdkUrl"))
			if err = child.evaluateScript(script.ToObject(child.vm), stringField(h.input, "sdkSource")); err != nil {
				return err
			}
			h.schedule(r, 0, 0, func() error {
				if child.retired {
					return nil
				}
				if err := r.dispatch(node, "load"); err != nil {
					return err
				}
				return child.dispatch(child.vm.GlobalObject(), "load")
			})
			return h.emit(map[string]any{"type": "frame", "url": src})
		}
		if h.kind == "parent" {
			h.activeFrame = child
			if err := h.emit(map[string]any{"type": "frame", "url": src}); err != nil {
				return err
			}
			parts := strings.Split(src, "/rch/")
			widget := ""
			if len(parts) > 1 {
				widget = strings.Split(parts[1], "/")[0]
			}
			h.schedule(r, 25*time.Millisecond, 0, func() error {
				for _, event := range []string{"init", "requestExtraParams"} {
					if err := h.queueMessage(r, child, map[string]any{"source": "cloudflare-challenge", "widgetId": widget, "event": event, "mode": "managed"}, src); err != nil {
						return err
					}
				}
				return nil
			})
		}
		return nil
	}
	if h.kind == "sentinel" && src == stringField(h.input, "sdkUrl") {
		h.schedule(r, 0, 0, func() error { return r.evaluateScript(node, stringField(h.input, "sdkSource")) })
		return nil
	}
	id := h.nextID()
	h.pending[id] = func(reply command) error {
		h.schedule(r, 0, 0, func() error {
			status := numberID(reply["status"])
			if status < 200 || status >= 300 {
				return r.dispatch(node, "error")
			}
			body := stringField(reply, "body")
			if raw := stringField(reply, "bodyBase64"); raw != "" {
				bytes, err := base64.StdEncoding.DecodeString(raw)
				if err != nil {
					return &Error{Code: "RESPONSE_BINARY_INVALID"}
				}
				body = string(bytes)
			}
			return r.evaluateScript(node, body)
		})
		return nil
	}
	return h.emit(map[string]any{"type": "request", "request": map[string]any{"id": id, "kind": "script", "method": "GET", "url": src, "sourceUrl": r.url, "headers": map[string]string{}}})
}
func (r *realm) evaluateScript(node *sobek.Object, source string) error {
	if err := r.runDocumentScript(node, "dynamic-script.js", source); err != nil {
		return err
	}
	return r.dispatch(node, "load")
}

func (r *realm) executableScript(node *sobek.Object) (bool, error) {
	get, _ := sobek.AssertFunction(node.Get("getAttribute"))
	value, err := get(node, r.vm.ToValue("type"))
	if err != nil {
		return false, err
	}
	if sobek.IsNull(value) {
		return true, nil
	}
	switch strings.ToLower(strings.TrimSpace(value.String())) {
	case "", "text/javascript", "application/javascript", "text/ecmascript", "application/ecmascript":
		return true, nil
	case "module":
		return false, &Error{Code: "JS_API_UNSUPPORTED_MODULE_SCRIPT"}
	default:
		return false, nil // Data scripts, including application/json, are inert.
	}
}

func (r *realm) runInitialScripts() error {
	// Snapshot the parsed nodes; scripts appended by a running script use the
	// insertion path instead. Each initial inline script has its own node.
	nodes, err := r.vm.RunString(`Array.from(document.querySelectorAll("script:not([src])"))`)
	if err != nil {
		return err
	}
	array := nodes.ToObject(r.vm)
	for i := 0; i < int(array.Get("length").ToInteger()); i++ {
		if err := r.inserted(array.Get(strconv.Itoa(i)).ToObject(r.vm)); err != nil {
			return err
		}
	}
	return nil
}

func (r *realm) runDocumentScript(node *sobek.Object, filename, source string) error {
	document := r.vm.Get("document").ToObject(r.vm)
	var scriptErr error
	// Sobek drains microtasks when the outermost JS call returns. Enter through
	// a JS trampoline so the nested RunScript still uses the global lexical
	// scope, but currentScript is restored before that microtask checkpoint.
	_, err := r.scriptEntry(sobek.Undefined(), r.vm.ToValue(func() {
		previous := document.Get("currentScript")
		_ = document.Set("currentScript", node)
		defer document.Set("currentScript", previous)
		_, scriptErr = r.vm.RunScript(filename, source)
	}))
	if scriptErr != nil {
		return scriptErr
	}
	return err
}
func (r *realm) dispatch(target sobek.Value, name string) error {
	fn, ok := sobek.AssertFunction(r.vm.Get("__dispatchEvent"))
	if !ok {
		return &Error{Code: "HOST_EVENT_UNAVAILABLE"}
	}
	_, err := fn(sobek.Undefined(), target, r.vm.ToValue(name))
	return err
}

func parseHTML(source, contextTag string) any {
	var nodes []*html.Node
	if contextTag != "" {
		contextTag = strings.ToLower(contextTag)
		context := &html.Node{Type: html.ElementNode, Data: contextTag, DataAtom: atom.Lookup([]byte(contextTag))}
		nodes, _ = html.ParseFragment(strings.NewReader(source), context)
	} else {
		node, err := html.Parse(strings.NewReader(source))
		if err == nil {
			return htmlTree(node)
		}
	}
	children := []any{}
	for _, node := range nodes {
		children = append(children, htmlTree(node))
	}
	return map[string]any{"nodeType": 11, "children": children}
}
func htmlTree(node *html.Node) any {
	nodeType := 0
	switch node.Type {
	case html.DocumentNode:
		nodeType = 9
	case html.ElementNode:
		nodeType = 1
	case html.TextNode:
		nodeType = 3
	case html.CommentNode:
		nodeType = 8
	case html.DoctypeNode:
		nodeType = 10
	}
	attrs := map[string]string{}
	for _, attr := range node.Attr {
		attrs[attr.Key] = attr.Val
	}
	children := []any{}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		children = append(children, htmlTree(child))
	}
	return map[string]any{"nodeType": nodeType, "tag": node.Data, "text": node.Data, "attrs": attrs, "children": children}
}
