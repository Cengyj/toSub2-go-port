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

func TestOversizedWorkerMessageFailsInsteadOfClosingSilently(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	input, err := json.Marshal(map[string]any{
		"url": "https://fixture.test/page", "loaderUrl": "https://fixture.test/loader.js",
		"loaderSource": `new Worker(URL.createObjectURL(new Blob(["postMessage('x'.repeat(33*1024*1024));"])));`,
	})
	if err != nil {
		t.Fatal(err)
	}
	commands, writer := io.Pipe()
	defer writer.Close()
	err = Run(ctx, "sentinel", input, commands, io.Discard)
	var diagnostic *Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != "WORKER_PROTOCOL_INVALID" {
		t.Fatalf("oversized worker message did not report a protocol failure: %v", err)
	}
	if ctx.Err() != nil || strings.Contains(err.Error(), "xxxx") {
		t.Fatal("worker message failed only at deadline or leaked message contents")
	}
}
