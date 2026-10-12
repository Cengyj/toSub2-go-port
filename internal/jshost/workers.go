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

	"github.com/grafana/sobek"
)

type blobResource struct {
	ownerOrigin, mime string
	bytes             []byte
}
type webWorker struct {
	id          int64
	owner       *realm
	target      *sobek.Object
	cancel      context.CancelFunc
	input       *io.PipeWriter
	encoder     *json.Encoder
	done        chan struct{}
	forwardDone chan struct{}
	writerDone  chan struct{}
	outbound    chan command
	stopped     bool // Only accessed by the owning host's event loop.
}

func (r *realm) effectiveOrigin() string {
	if value := stringField(r.config, "workerOrigin"); value != "" {
		return value
	}
	return origin(r.url)
}

func (r *realm) installWorkers(native *sobek.Object) {
	vm, h := r.vm, r.host
	_ = native.Set("blobURL", func(call sobek.FunctionCall) sobek.Value {
		bytes, ok := call.Argument(0).Export().(sobek.ArrayBuffer)
		if !ok {
			panic(vm.NewTypeError("Expected Blob bytes"))
		}
		if h.blobs == nil {
			h.blobs = map[string]blobResource{}
		}
		if len(h.blobs) >= 1024 {
			panic(vm.NewTypeError("JS_RESOURCE_LIMIT_BLOBS"))
		}
		key := "blob:" + r.effectiveOrigin() + "/go-" + strconv.FormatInt(h.nextID(), 10)
		h.blobs[key] = blobResource{ownerOrigin: r.effectiveOrigin(), mime: call.Argument(1).String(), bytes: append([]byte(nil), bytes.Bytes()...)}
		return vm.ToValue(key)
	})
	_ = native.Set("revokeBlobURL", func(key string) {
		if resource, ok := h.blobs[key]; ok && resource.ownerOrigin == r.effectiveOrigin() {
			delete(h.blobs, key)
		}
	})
	_ = native.Set("workerStart", func(call sobek.FunctionCall) sobek.Value {
		if len(h.workers) >= 32 || len(h.allWorkers) >= 128 {
			panic(vm.NewTypeError("JS_RESOURCE_LIMIT_WORKERS"))
		}
		workerURL := call.Argument(1).String()
		if call.Argument(2).String() != "classic" {
			panic(vm.NewTypeError("JS_API_UNSUPPORTED_MODULE_WORKER"))
		}
		config := map[string]any{}
		for k, v := range r.config {
			config[k] = v
		}
		delete(config, "workerSource")
		config["url"] = workerURL
		config["workerOrigin"] = r.effectiveOrigin()
		config["workerName"] = call.Argument(3).String()
		config["networkURL"] = workerURL
		if strings.HasPrefix(workerURL, "blob:") {
			config["networkURL"] = r.url
			if existing := stringField(r.config, "networkURL"); existing != "" {
				config["networkURL"] = existing
			}
		}
		if strings.HasPrefix(workerURL, "blob:") {
			resource, ok := h.blobs[workerURL]
			if !ok || resource.ownerOrigin != r.effectiveOrigin() {
				panic(vm.NewTypeError("Worker source unavailable"))
			}
			config["workerSource"] = string(resource.bytes)
		} else if origin(workerURL) != r.effectiveOrigin() {
			panic(vm.NewTypeError("SecurityError: cross-origin worker"))
		}
		// Blob contents are copied into the new host. A worker never shares Go
		// maps or Sobek values with its creator; all communication uses NDJSON.
		available := map[string]any{}
		for key, resource := range h.blobs {
			if resource.ownerOrigin == r.effectiveOrigin() {
				available[key] = base64.StdEncoding.EncodeToString(resource.bytes)
			}
		}
		config["workerBlobs"] = available
		data, err := json.Marshal(config)
		if err != nil {
			panic(vm.NewTypeError("Worker input invalid"))
		}
		ctx, cancel := context.WithCancel(h.ctx)
		commands, input := io.Pipe()
		events, output := io.Pipe()
		// Cancellation must also interrupt a creator blocked writing to a busy
		// worker's bounded command queue. VM.Interrupt alone cannot stop Go I/O.
		context.AfterFunc(ctx, func() { _ = input.CloseWithError(ctx.Err()) })
		worker := &webWorker{owner: r, target: call.Argument(0).ToObject(vm), cancel: cancel, input: input, encoder: json.NewEncoder(input), done: make(chan struct{}), forwardDone: make(chan struct{}), writerDone: make(chan struct{}), outbound: make(chan command, 128)}
		id := h.nextID()
		worker.id = id
		if h.workers == nil {
			h.workers = map[int64]*webWorker{}
		}
		h.workers[id] = worker
		h.allWorkers = append(h.allWorkers, worker)
		if h.async == nil {
			h.async = make(chan func() error, 64)
		}
		go func() {
			defer close(worker.done)
			defer output.Close()
			defer commands.Close()
			if err := Run(ctx, "webworker", data, commands, output); err != nil && ctx.Err() == nil {
				_ = json.NewEncoder(output).Encode(map[string]any{"type": "failure", "code": safeWorkerCode(err)})
			}
		}()
		go worker.forward(ctx, events)
		go worker.writeCommands(ctx)
		return vm.ToValue(id)
	})
	_ = native.Set("workerPost", func(call sobek.FunctionCall) sobek.Value {
		worker := h.workers[call.Argument(0).ToInteger()]
		if worker == nil || worker.stopped {
			return sobek.Undefined()
		}
		if err := worker.send(command{"type": "worker-message", "graph": r.messageCopy(call.Argument(1)).graph}); err != nil {
			panic(vm.NewTypeError(safeWorkerCode(err)))
		}
		return sobek.Undefined()
	})
	_ = native.Set("workerStop", func(id int64) {
		if worker := h.workers[id]; worker != nil {
			worker.stop()
			delete(h.workers, id)
		}
	})
	_ = native.Set("workerClose", func() {
		if h.kind == "webworker" {
			h.finished = true
		}
	})
	_ = native.Set("importScript", func(raw string) {
		if h.kind != "webworker" {
			panic(vm.NewTypeError("importScripts is only available in workers"))
		}
		base, _ := url.Parse(r.url)
		ref, err := url.Parse(raw)
		if err != nil {
			panic(vm.NewTypeError("Invalid script URL"))
		}
		source, err := r.workerSource(base.ResolveReference(ref).String())
		if err != nil {
			panic(vm.NewTypeError(safeWorkerCode(err)))
		}
		if _, err = vm.RunScript("imported-worker-script.js", source); err != nil {
			panic(vm.NewTypeError(safeWorkerCode(err)))
		}
	})
}

func safeWorkerCode(err error) string {
	if typed, ok := err.(*Error); ok {
		return typed.Code
	}
	return "WORKER_SCRIPT_EXCEPTION"
}
func (w *webWorker) send(value command) error {
	select {
	case <-w.done:
		return nil
	default:
	}
	select {
	case w.outbound <- value:
		return nil
	default:
		return &Error{Code: "JS_RESOURCE_LIMIT_WORKER_MESSAGES"}
	}
}
func (w *webWorker) writeCommands(ctx context.Context) {
	defer close(w.writerDone)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.done:
			return
		case value := <-w.outbound:
			if w.encoder.Encode(value) != nil {
				return
			}
		}
	}
}
func (w *webWorker) stop() {
	if w.stopped {
		return
	}
	w.stopped = true
	w.cancel()
	_ = w.input.Close()
}

func (w *webWorker) forward(ctx context.Context, reader *io.PipeReader) {
	defer close(w.forwardDone)
	defer reader.Close()
	defer func() {
		select {
		case <-ctx.Done():
			return
		case w.owner.host.async <- func() error {
			// Natural close follows already-posted messages in task order.
			// Immediate stop here could discard the last message when another
			// timer runs between decoding that message and dispatching it.
			w.owner.host.schedule(w.owner, 0, 0, func() error {
				w.stop()
				delete(w.owner.host.workers, w.id)
				return nil
			})
			return nil
		}:
		}
	}()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 65536), maxMessageBytes)
	for scanner.Scan() {
		var event command
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case w.owner.host.async <- func() error {
			if w.stopped {
				return nil
			}
			h, r := w.owner.host, w.owner
			switch stringField(event, "type") {
			case "worker-message":
				h.schedule(r, 0, 0, func() error {
					if w.stopped {
						return nil
					}
					unpack, _ := sobek.AssertFunction(r.vm.Get("__cloneUnpack"))
					value, err := unpack(sobek.Undefined(), r.vm.ToValue(event["graph"]))
					if err != nil {
						return err
					}
					dispatch, _ := sobek.AssertFunction(r.vm.Get("__workerMessage"))
					_, err = dispatch(sobek.Undefined(), w.target, value)
					return err
				})
				return nil
			case "failure":
				w.stop()
				dispatch, _ := sobek.AssertFunction(r.vm.Get("__workerError"))
				_, err := dispatch(sobek.Undefined(), w.target, r.vm.ToValue(stringField(event, "code")))
				return err
			case "request", "cookie":
				var oldID any
				id := h.nextID()
				if stringField(event, "type") == "request" {
					request, ok := event["request"].(map[string]any)
					if !ok {
						return &Error{Code: "WORKER_PROTOCOL_INVALID"}
					}
					oldID = request["id"]
					request["id"] = id
				} else {
					oldID = event["id"]
					event["id"] = id
				}
				h.pending[id] = func(reply command) error {
					if w.stopped {
						return nil
					}
					reply["id"] = oldID
					return w.send(reply)
				}
				return h.emit(event)
			default:
				return &Error{Code: "WORKER_PROTOCOL_INVALID"}
			}
		}:
		}
	}
}

func (r *realm) workerSource(rawURL string) (string, error) {
	if strings.HasPrefix(rawURL, "blob:") {
		if sources, ok := r.config["workerBlobs"].(map[string]any); ok {
			if encoded, ok := sources[rawURL].(string); ok {
				bytes, err := base64.StdEncoding.DecodeString(encoded)
				if err == nil {
					return string(bytes), nil
				}
			}
		}
		return "", &Error{Code: "WORKER_SOURCE_UNAVAILABLE"}
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", &Error{Code: "WORKER_SOURCE_INVALID"}
	}
	h := r.host
	id := h.nextID()
	if err := h.emit(map[string]any{"type": "request", "request": map[string]any{"id": id, "kind": "script", "method": "GET", "url": rawURL, "sourceUrl": rawURL, "headers": map[string]string{}}}); err != nil {
		return "", err
	}
	for {
		select {
		case <-h.ctx.Done():
			return "", h.ctx.Err()
		case event := <-h.commands:
			if event.err != nil {
				return "", &Error{Code: "WORKER_INPUT_CLOSED"}
			}
			if numberID(event.command["id"]) == id {
				status := numberID(event.command["status"])
				if status < 200 || status >= 300 {
					return "", &Error{Code: "WORKER_SOURCE_HTTP"}
				}
				if encoded, ok := event.command["bodyBase64"].(string); ok {
					bytes, err := base64.StdEncoding.DecodeString(encoded)
					if err != nil {
						return "", &Error{Code: "RESPONSE_BINARY_INVALID"}
					}
					return string(bytes), nil
				}
				return stringField(event.command, "body"), nil
			}
			h.deferred = append(h.deferred, event)
		}
	}
}
