package legacyruntime

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"tosub2-go-port/internal/jshost"
	"tosub2-go-port/internal/transport"
)

// A script worker executes dynamic JavaScript, while the owning Go session
// retains all HTTP, cookies, deadlines, private inputs and process-tree cleanup.
type scriptRuntime struct {
	cmd          *exec.Cmd
	tree         *processTree
	stdin        io.WriteCloser
	stdout       *os.File
	input        string
	cancel       context.CancelFunc
	done         chan struct{}
	readDone     chan struct{}
	terminalDone chan struct{}
	stopOnce     sync.Once
	mu           sync.Mutex
	writeMu      sync.Mutex
	err          error
	exitErr      error
	terminalErr  error
}

type runtimeEvent struct {
	source  string
	runtime *scriptRuntime
	message runtimeMessage
	err     error
}

type runtimeMessage struct {
	Type       string            `json:"type"`
	ID         json.RawMessage   `json:"id"`
	Action     string            `json:"action"`
	Code       string            `json:"code"`
	URL        string            `json:"url"`
	Method     string            `json:"method"`
	Flow       string            `json:"flow"`
	Value      *string           `json:"value"`
	Errors     []json.RawMessage `json:"errors"`
	ErrorCount int               `json:"errorCount"`
	Result     struct {
		Errors         []json.RawMessage `json:"errors"`
		ReportedErrors []json.RawMessage `json:"reportedErrors"`
	} `json:"result"`
	Request generatedRequest `json:"request"`
	Message struct {
		Data  json.RawMessage `json:"data"`
		Graph json.RawMessage `json:"graph,omitempty"`
	} `json:"message"`
}

type generatedRequest struct {
	ID         json.RawMessage   `json:"id"`
	Kind       string            `json:"kind"`
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	SourceURL  string            `json:"sourceUrl"`
	Body       string            `json:"body"`
	BodyBase64 *string           `json:"bodyBase64,omitempty"`
	Headers    map[string]string `json:"headers"`
}

// startRuntime executes authentication scripts only in the Go worker.
func (c *Client) startRuntime(ctx context.Context, kind string, input any, events chan<- runtimeEvent) (*scriptRuntime, error) {
	if kind != "sentinel" && kind != "parent" && kind != "child" {
		return nil, ErrProtocol
	}
	worker := c.opts.WorkerPath
	if worker == "" {
		var err error
		worker, err = os.Executable()
		if err != nil {
			return nil, ErrUnavailable
		}
	}
	return startProcessRuntime(ctx, worker, []string{"--auth-js-worker", kind}, kind, input, events)
}

func startProcessRuntime(ctx context.Context, executable string, arguments []string, source string, input any, events chan<- runtimeEvent) (*scriptRuntime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return nil, ErrProtocol
	}
	f, err := os.CreateTemp("", "tosub2-js-*.json")
	if err != nil {
		return nil, errors.New("LEGACY_RUNTIME_INPUT: cannot create private runtime input")
	}
	path := f.Name()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, errors.New("LEGACY_RUNTIME_INPUT: cannot write private runtime input")
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, errors.New("LEGACY_RUNTIME_INPUT: cannot close private runtime input")
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &scriptRuntime{input: path, cancel: cancel, done: make(chan struct{}), readDone: make(chan struct{}), terminalDone: make(chan struct{})}
	r.cmd = exec.CommandContext(ctx, executable, append(arguments, path)...)
	r.cmd.Stderr = io.Discard // Never expose remote scripts, URLs or secrets.
	r.tree, err = prepareProcessTree(r.cmd)
	if err != nil {
		cancel()
		_ = os.Remove(path)
		return nil, ErrUnavailable
	}
	r.stdin, err = r.cmd.StdinPipe()
	if err != nil {
		cancel()
		_ = r.tree.terminate(nil)
		_ = os.Remove(path)
		return nil, ErrUnavailable
	}
	stdout, writer, err := os.Pipe()
	if err != nil {
		cancel()
		_ = r.stdin.Close()
		_ = r.tree.terminate(nil)
		_ = os.Remove(path)
		return nil, ErrUnavailable
	}
	r.stdout = stdout
	r.cmd.Stdout = writer
	if err = r.cmd.Start(); err != nil {
		cancel()
		_ = writer.Close()
		_ = stdout.Close()
		_ = r.stdin.Close()
		_ = r.tree.terminate(nil)
		_ = os.Remove(path)
		return nil, ErrUnavailable
	}
	_ = writer.Close()
	if err = r.tree.attach(r.cmd.Process); err != nil {
		cancel()
		_ = r.tree.terminate(r.cmd.Process)
		_ = r.stdin.Close()
		_ = stdout.Close()
		_ = r.cmd.Wait()
		_ = os.Remove(path)
		return nil, ErrUnavailable
	}
	go func() {
		waitErr := r.cmd.Wait()
		_ = r.tree.terminate(r.cmd.Process)
		cleanup := r.tree.finalError(r.cmd.Process)
		r.mu.Lock()
		// Never expose exec.ExitError, which may contain executable paths or
		// signal text. Only the Go worker's fixed statuses become diagnostics.
		var exit *exec.ExitError
		if ctx.Err() == nil && errors.As(waitErr, &exit) {
			if code := jshost.WorkerExitDiagnostic(exit.ExitCode()); code != "" {
				r.exitErr = runtimeFailure(runtimeMessage{Code: code})
			}
		}
		if cleanup != nil {
			r.err = errors.New("LEGACY_RUNTIME_CLEANUP: script worker process tree could not be terminated")
		}
		r.mu.Unlock()
		close(r.done)
	}()
	go func() {
		defer close(r.readDone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
		for scanner.Scan() {
			var message runtimeMessage
			if json.Unmarshal(scanner.Bytes(), &message) != nil || message.Type == "" {
				r.recordTerminalFailure(ErrProtocol)
				select {
				case events <- runtimeEvent{source: source, runtime: r, err: ErrProtocol}:
				case <-ctx.Done():
				}
				r.stop()
				return
			}
			if message.Type == "failure" {
				// The owner may still be replying to an earlier request when the
				// worker exits. Cache only the existing allowlisted diagnosis before
				// forwarding, so a full event queue cannot hide it behind EOF/EPIPE.
				r.recordTerminalFailure(runtimeFailure(message))
			}
			select {
			case events <- runtimeEvent{source: source, runtime: r, message: message}:
			case <-ctx.Done():
				return
			}
		}
		if scanner.Err() != nil {
			r.recordTerminalFailure(ErrProtocol)
			select {
			case events <- runtimeEvent{source: source, runtime: r, err: ErrProtocol}:
			case <-ctx.Done():
			}
			r.stop()
			return
		}
		// Stdout can close just before Wait publishes a fixed exit status.
		// Keep the original cancellation path while waiting for classification.
		select {
		case <-r.done:
		case <-ctx.Done():
			return
		}
		failure := r.processFailure(ctx, "script worker exited before completing the operation")
		r.recordTerminalFailure(failure)
		select {
		case events <- runtimeEvent{source: source, runtime: r, err: failure}:
		case <-ctx.Done():
		}
	}()
	return r, nil
}

func (r *scriptRuntime) stop() {
	r.stopOnce.Do(func() { r.cancel(); _ = r.stdin.Close(); _ = r.tree.terminate(r.cmd.Process) })
}

func (r *scriptRuntime) close() (err error) {
	if r == nil {
		return nil
	}
	defer func() {
		if removeErr := os.Remove(r.input); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, errors.New("LEGACY_RUNTIME_CLEANUP: private script input could not be removed"))
		}
	}()
	r.stop()
	_ = r.stdout.Close()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		return errors.New("LEGACY_RUNTIME_CLEANUP: script worker did not exit before cleanup deadline")
	}
	r.mu.Lock()
	err = r.err
	r.mu.Unlock()
	select {
	case <-r.readDone:
	case <-time.After(time.Second):
		err = errors.Join(err, errors.New("LEGACY_RUNTIME_CLEANUP: script output reader did not stop"))
	}
	return err
}

func (c *Client) closeRuntimes(runtimes ...*scriptRuntime) error {
	var cleanup error
	for _, runtime := range runtimes {
		cleanup = errors.Join(cleanup, runtime.close())
	}
	if cleanup != nil {
		c.mu.Lock()
		c.cleanupErr = errors.Join(c.cleanupErr, cleanup)
		c.mu.Unlock()
	}
	return cleanup
}

func (r *scriptRuntime) send(ctx context.Context, message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return ErrProtocol
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	written := make(chan error, 1)
	go func() {
		r.writeMu.Lock()
		defer r.writeMu.Unlock()
		_, err := r.stdin.Write(append(data, '\n'))
		written <- err
	}()
	select {
	case <-writeCtx.Done():
		r.stop()
		return writeCtx.Err()
	case <-r.done:
		return r.exitedSendFailure(writeCtx, "script worker exited before receiving a response")
	case err := <-written:
		if err != nil {
			// A closed pipe can precede Wait's exit notification. Give a
			// terminating worker a brief bounded chance to publish its fixed
			// resource diagnosis without delaying a live broken-pipe worker.
			select {
			case <-r.terminalDone:
				return r.processFailure(ctx, "cannot write to script worker")
			case <-r.done:
				return r.exitedSendFailure(writeCtx, "cannot write to script worker")
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
			return r.processFailure(ctx, "cannot write to script worker")
		}
		return nil
	}
}

func (r *scriptRuntime) recordTerminalFailure(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminalErr == nil {
		r.terminalErr = err
		close(r.terminalDone)
	}
}

func (r *scriptRuntime) exitedSendFailure(ctx context.Context, message string) error {
	// Wait can finish before the stdout reader sees a final failure. Preserve
	// that diagnostic without extending the existing send/caller deadline.
	select {
	case <-r.terminalDone:
	case <-r.readDone:
	case <-ctx.Done():
	}
	return r.processFailure(ctx, message)
}

func (r *scriptRuntime) processFailure(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exitErr != nil {
		return r.exitErr
	}
	if r.terminalErr != nil {
		return r.terminalErr
	}
	return errors.New("LEGACY_RUNTIME_FAILED: " + message)
}

func runtimeHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return nil, errors.New("LEGACY_RUNTIME_URL: invalid HTTP runtime URL")
	}
	return u, nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func (c *Client) runtimeHeaders(page, target string, generated generatedRequest, sentinel bool) (http.Header, error) {
	source, err := runtimeHTTPURL(page)
	if err != nil {
		return nil, err
	}
	destination, err := runtimeHTTPURL(target)
	if err != nil {
		return nil, err
	}
	h := make(http.Header)
	for key, value := range generated.Headers {
		h.Set(key, value)
	}
	defaults := map[string]string{"User-Agent": c.UserAgent(), "Accept-Language": c.AcceptLanguage(), "Referer": page, "Accept": "*/*", "Sec-Fetch-Site": "cross-site"}
	if sameOrigin(source, destination) {
		defaults["Sec-Fetch-Site"] = "same-origin"
	}
	ident := identity(c.Profile())
	for key, name := range map[string]string{"secChUa": "Sec-Ch-Ua", "secChUaMobile": "Sec-Ch-Ua-Mobile", "secChUaPlatform": "Sec-Ch-Ua-Platform"} {
		if value, ok := ident[key].(string); ok && value != "" {
			defaults[name] = value
		}
	}
	if sentinel {
		defaults["Sec-Fetch-Mode"] = "cors"
		defaults["Sec-Fetch-Dest"] = "empty"
		if sameOrigin(source, destination) && strings.HasSuffix(destination.Path, "/req") {
			defaults["Origin"] = destination.Scheme + "://" + destination.Host
			defaults["Content-Type"] = "text/plain;charset=UTF-8"
		}
	} else if generated.Kind == "script" {
		defaults["Sec-Fetch-Mode"] = "no-cors"
		defaults["Sec-Fetch-Dest"] = "script"
	} else {
		defaults["Sec-Fetch-Mode"] = "cors"
		defaults["Sec-Fetch-Dest"] = "empty"
		if strings.ToUpper(generated.Method) != "GET" {
			defaults["Origin"] = source.Scheme + "://" + source.Host
			defaults["Content-Type"] = "text/plain;charset=UTF-8"
		}
	}
	for key, value := range defaults {
		if h.Get(key) == "" {
			h.Set(key, value)
		}
	}
	return curlRuntimeDefaults(h), nil
}

// The original dynamic solvers inherit curl's Chrome default headers. Ordinary
// protocol requests may intentionally omit them, so apply these only here.
func curlRuntimeDefaults(h http.Header) http.Header {
	if h == nil {
		h = make(http.Header)
	}
	for name, value := range map[string]string{
		"Accept-Encoding":           "gzip, deflate, br, zstd",
		"Priority":                  "u=0, i",
		"Upgrade-Insecure-Requests": "1",
		"Sec-Fetch-User":            "?1",
	} {
		if h.Get(name) == "" {
			h.Set(name, value)
		}
	}
	return h
}

func (c *Client) submitRuntimeRequest(ctx context.Context, page string, generated generatedRequest, sentinel bool) (map[string]any, error) {
	if len(generated.ID) == 0 || string(generated.ID) == "null" {
		return nil, ErrProtocol
	}
	h, err := c.runtimeHeaders(page, generated.URL, generated, sentinel)
	if err != nil {
		return nil, err
	}
	method := strings.ToUpper(generated.Method)
	if method == "" {
		method = "GET"
	}
	var body []byte
	if generated.BodyBase64 != nil {
		if strings.ContainsAny(*generated.BodyBase64, "\r\n") {
			return nil, ErrProtocol
		}
		body, err = base64.StdEncoding.Strict().DecodeString(*generated.BodyBase64)
		if err != nil {
			return nil, ErrProtocol
		}
	} else if method != "GET" && method != "HEAD" {
		body = []byte(generated.Body)
	}
	if method == "GET" || method == "HEAD" {
		body = nil
	}
	// Script fetches share the protocol policy: reads can recover after a lost
	// response, while writes only retry an interrupted dial or TLS handshake
	// that the transport proves never reached an HTTP-capable connection.
	response, err := c.runtimeRequest(ctx, transport.Request{Method: method, URL: generated.URL, Headers: h, Body: body, Timeout: 45 * time.Second, RetryBeforeSend: method != http.MethodGet && method != http.MethodHead})
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for name, values := range response.Header {
		headers[strings.ToLower(name)] = strings.Join(values, ", ")
	}
	responseURL := generated.URL
	if response.URL != nil {
		responseURL = response.URL.String()
	}
	// Always preserve raw bytes; content-type labels do not reliably identify
	// binary WASM, fonts or downloaded challenge resources. The text field is
	// retained for script loaders that consume source directly.
	result := map[string]any{"id": generated.ID, "status": response.StatusCode, "url": responseURL, "headers": headers, "body": string(response.Body), "bodyBase64": base64.StdEncoding.EncodeToString(response.Body)}
	return result, nil
}

func runtimeFailure(message runtimeMessage) error {
	if !jshost.IsDiagnosticCode(message.Code) && message.Code != "EXECUTION_FAILED" {
		return ErrProtocol
	}
	return errors.New("GO_JS_RUNTIME: " + message.Code)
}

// Script cookies use the authoritative session jar, including server updates
// and deletions. HttpOnly values are never sent into the JavaScript realm.
func (c *Client) submitRuntimeCookie(message runtimeMessage) (map[string]any, error) {
	var id uint64
	if json.Unmarshal(message.ID, &id) != nil || id == 0 || id > (1<<53)-1 {
		return nil, ErrProtocol
	}
	if _, err := runtimeHTTPURL(message.URL); err != nil {
		return nil, ErrProtocol
	}
	jar := c.cookieJar()
	switch message.Action {
	case "get":
	case "set":
		if message.Value == nil {
			return nil, ErrProtocol
		}
		jar.SetDocumentCookie(message.URL, *message.Value)
	default:
		return nil, ErrProtocol
	}
	return map[string]any{"type": "cookie-result", "id": message.ID, "value": jar.DocumentCookie(message.URL)}, nil
}

// Asset/frame GETs follow redirects, but generated mutation requests do not.
// SDK redirects remain on the loader origin just like its discovered SDK URL.
func (c *Client) runtimeGet(ctx context.Context, rawURL string, headers http.Header, redirects bool, origin *url.URL) (*transport.Response, error) {
	headers = curlRuntimeDefaults(headers.Clone())
	for attempt := 0; attempt < 10; attempt++ {
		u, err := runtimeHTTPURL(rawURL)
		if err != nil {
			return nil, err
		}
		if origin != nil && !sameOrigin(origin, u) {
			return nil, errors.New("SENTINEL_SDK_ORIGIN: SDK must use the configured loader origin")
		}
		response, err := c.runtimeRequest(ctx, transport.Request{Method: http.MethodGet, URL: rawURL, Headers: headers, Timeout: 30 * time.Second})
		if err != nil {
			return nil, err
		}
		if !redirects || response.StatusCode < 300 || response.StatusCode >= 400 || response.Header.Get("Location") == "" {
			return response, nil
		}
		location, err := url.Parse(response.Header.Get("Location"))
		if err != nil {
			return nil, ErrProtocol
		}
		rawURL = u.ResolveReference(location).String()
	}
	return nil, errors.New("LEGACY_RUNTIME_REDIRECT: runtime resource exceeded redirect limit")
}
