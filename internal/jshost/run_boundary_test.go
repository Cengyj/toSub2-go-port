package jshost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type runBoundaryWriter func([]byte) (int, error)

func (f runBoundaryWriter) Write(data []byte) (int, error) { return f(data) }

func runGetterFixture(ctx context.Context, getter string, events io.Writer) (err error, escaped any) {
	defer func() { escaped = recover() }()
	input, _ := json.Marshal(map[string]any{
		"url": "https://fixture.test/", "loaderUrl": "https://fixture.test/loader.js",
		"loaderSource": `Object.defineProperty(globalThis,'SentinelSDK',{get(){` + getter + `}});`,
	})
	err = Run(ctx, "sentinel", input, strings.NewReader("{\"type\":\"call\",\"method\":\"token\",\"flow\":\"fixture\"}\n"), events)
	return
}

func TestRunCancellationInsideHostGetterReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	observed := false
	writer := runBoundaryWriter(func(data []byte) (int, error) {
		var event map[string]any
		if json.Unmarshal(data, &event) == nil && event["type"] == "cookie" {
			observed = true
			cancel()
		}
		return len(data), nil
	})
	err, escaped := runGetterFixture(ctx, `try{document.cookie}catch(error){}while(true){}`, writer)
	if !observed || escaped != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("host getter cancellation did not return its context error", "observed", observed, "panic", escaped != nil)
	}
}

func TestRunHostGetterExceptionIsPrivate(t *testing.T) {
	err, escaped := runGetterFixture(t.Context(), `throw new Error('`+privateDiagnosticFixture+`');`, io.Discard)
	var diagnostic *Error
	if escaped != nil || !errors.As(err, &diagnostic) || diagnostic.Code != "SCRIPT_EXCEPTION" || strings.Contains(err.Error(), privateDiagnosticFixture) {
		t.Fatal("script getter exception escaped the private host error boundary")
	}
}

func TestRunCancellationDoesNotHideUnexpectedHostPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	marker := errors.New("synthetic host implementation panic")
	writer := runBoundaryWriter(func([]byte) (int, error) { cancel(); panic(marker) })
	_, escaped := runGetterFixture(ctx, `document.cookie;`, writer)
	if escaped != marker {
		t.Fatal("context cancellation hid an unexpected host panic")
	}
}
