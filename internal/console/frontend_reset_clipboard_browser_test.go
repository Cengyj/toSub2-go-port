package console

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanw/esbuild/pkg/api"
)

var resetBrowser = flag.String("reset-browser", "", "opt-in: absolute path to a local Chromium executable; uses a new temporary profile")

// Mount the actual log panel with vendored production React. Capture the
// writeText argument on loopback instead of touching the user's clipboard.
func requireResetClipboardBrowser(t *testing.T) {
	t.Helper()
	if *resetBrowser == "" || *resetPages == "" {
		t.Skip("opt-in real browser requires a fresh synthetic API page and Chromium")
	}
	if !filepath.IsAbs(*resetBrowser) {
		t.Fatal("browser path must be absolute")
	}
}

func TestFrontendResetClipboardInBrowser(t *testing.T) {
	requireResetClipboardBrowser(t)
	for _, c := range []struct {
		name, wantFile string
		warnings       bool
	}{
		{"old", "clipboard-receipt.txt", false},
		{"new-not-visible", "clipboard.txt", false},
		{"new-and-old-logout", "clipboard-warning.txt", true},
		{"empty-logout-unconfirmed", "clipboard-logout-unconfirmed.txt", true},
		{"activation-mixed-signals-logout", "clipboard-mixed-signals.txt", true},
	} {
		t.Run(c.name, func(t *testing.T) { verifyBrowserResetCopy(t, c.name, c.wantFile, c.warnings) })
	}
}

func TestFrontendLocalResetClipboardInBrowser(t *testing.T) {
	requireResetClipboardBrowser(t)
	verifyBrowserResetCopy(t, "legacy-binding-read", "clipboard-local-readback.txt", true)
}

func verifyBrowserResetCopy(t *testing.T, scenario, wantFile string, warnings bool) {
	t.Helper()
	page, err := os.ReadFile(filepath.Join(*resetPages, scenario+".json"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(*resetPages, wantFile))
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		ID      string `json:"id"`
		Entries []struct {
			OperationAt string `json:"operationAt"`
			Status      string `json:"status"`
			Diagnostic  struct {
				Terminal bool `json:"terminal"`
			} `json:"reset_diagnostic"`
		} `json:"entries"`
	}
	if json.Unmarshal(page, &data) != nil || data.ID != "session-loss-audit" {
		t.Fatal("not a synthetic fixture page")
	}
	at := ""
	for _, r := range data.Entries {
		if r.Diagnostic.Terminal {
			at = r.OperationAt
		}
	}
	if at == "" {
		t.Fatal("fixture lacks terminal record")
	}
	status := "failed"
	for _, entry := range data.Entries {
		if entry.OperationAt == at && entry.Status != "" {
			status = entry.Status
		}
	}
	props, _ := json.Marshal(map[string]any{"token": "test-only", "jobId": data.ID, "job": map[string]any{"status": status, "lastOperationAt": at}})
	web, err := filepath.Abs("../../web")
	if err != nil {
		t.Fatal(err)
	}
	alias := map[string]string{"react": "react/react.production.js", "react-dom": "react-dom/react-dom.production.js", "react-dom/client": "react-dom/react-dom-client.production.js", "lucide-react": "lucide-react/index.js", "scheduler": "scheduler/scheduler.production.js"}
	for k, v := range alias {
		alias[k] = filepath.Join(web, "vendor", v)
	}
	js := `import React from "react";import {createRoot} from "react-dom/client";import {JobLogs} from "./src/components/job-logs.jsx";
Object.defineProperty(navigator,"clipboard",{value:{writeText:async text=>{await fetch("/capture",{method:"POST",body:text});}}});
createRoot(document.getElementById("root")).render(React.createElement(JobLogs,` + string(props) + `));
const poll=setInterval(()=>{const button=document.querySelector('button[aria-label="复制当前显示的日志"]');if(!button||button.disabled||!document.querySelector('summary')||!document.body.textContent.includes('本轮结果：'))return;
if(` + strconv.FormatBool(warnings) + `){const filter=[...document.querySelectorAll('button')].find(b=>b.textContent.startsWith('异常'));if(!filter)return;if(filter.getAttribute('aria-pressed')!=='true'){filter.click();return;}}
clearInterval(poll);button.click();},30);`
	built := api.Build(api.BuildOptions{AbsWorkingDir: web, Stdin: &api.StdinOptions{Contents: js, ResolveDir: web, Loader: api.LoaderJSX}, Bundle: true, Alias: alias, Loader: map[string]api.Loader{".css": api.LoaderEmpty}, Define: map[string]string{"process.env.NODE_ENV": `"production"`}, LogLevel: api.LogLevelSilent})
	if len(built.Errors) > 0 || len(built.OutputFiles) != 1 {
		t.Fatal("could not bundle log panel", built.Errors)
	}
	var copied string
	capture := make(chan string, 1)
	var scripts, pages atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'")
		io.WriteString(w, `<!doctype html><meta charset=utf-8><div id="root"></div><script src="/test.js"></script>`)
	})
	mux.HandleFunc("GET /test.js", func(w http.ResponseWriter, _ *http.Request) {
		scripts.Add(1)
		w.Header().Set("Content-Type", "text/javascript")
		w.Write(built.OutputFiles[0].Contents)
	})
	mux.HandleFunc("GET /api/jobs/"+data.ID+"/logs", func(w http.ResponseWriter, _ *http.Request) {
		pages.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write(page)
	})
	mux.HandleFunc("POST /capture", func(w http.ResponseWriter, r *http.Request) {
		b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if e != nil {
			w.WriteHeader(400)
			return
		}
		select {
		case capture <- string(b):
		default:
		}
		w.WriteHeader(204)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, *resetBrowser, "--headless", "--no-first-run", "--no-default-browser-check", "--disable-sync", "--disable-extensions", "--disable-background-networking", "--disable-component-update", "--password-store=basic", "--use-mock-keychain", "--proxy-server=http://127.0.0.1:9", "--proxy-bypass-list=127.0.0.1", "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1, EXCLUDE localhost", "--user-data-dir="+t.TempDir(), "--dump-dom", "--virtual-time-budget=10000", server.URL)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	dom, e := cmd.Output()
	if e != nil {
		t.Fatal("isolated Chromium failed", e)
	}
	select {
	case copied = <-capture:
	default:
		t.Fatalf("browser did not copy: script loads=%d log reads=%d button=%t terminal=%t", scripts.Load(), pages.Load(), bytes.Contains(dom, []byte("复制当前显示的日志")), bytes.Contains(dom, []byte("本轮结果：")))
	}
	if copied != strings.TrimSuffix(string(want), "\n") {
		t.Fatal("real browser and component clipboard output differ")
	}
}
