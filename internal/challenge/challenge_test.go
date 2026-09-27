package challenge

import (
	"context"
	"strings"
	"testing"
)

type fakeBrowser struct{ evaluated string }

func (f *fakeBrowser) Navigate(context.Context, string) error { return nil }
func (f *fakeBrowser) Cookies() ([]map[string]any, error)     { return nil, nil }
func (f *fakeBrowser) SetCookies([]map[string]any) error      { return nil }
func (f *fakeBrowser) Evaluate(_ context.Context, script string) (any, error) {
	f.evaluated = script
	if strings.Contains(script, "__tosub2SentinelRuntime;") {
		return nil, nil
	}
	return map[string]any{"token": `{"p":"fixture","t":"fixture","c":"fixture","id":"device","flow":"email_otp_validate"}`, "so": `{"so":"fixture","c":"fixture","id":"device","flow":"email_otp_validate"}`, "observer": "available"}, nil
}
func (f *fakeBrowser) EvaluateFrame(context.Context, string, string) (any, error) { return "", nil }
func (f *fakeBrowser) HTML(context.Context) (string, error)                       { return "", nil }
func (f *fakeBrowser) Close() error                                               { return nil }

func TestSentinelUsesBrowserSessionAndReturnsBoundTokens(t *testing.T) {
	f := &fakeBrowser{}
	r, err := (BrowserSolver{Session: f}).Sentinel(context.Background(), "https://auth.example/log-in", "https://sentinel.example/loader.js", "email_otp_validate")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Solved || r.Provider != "sentinel" || !r.SessionBound || r.Token == "" || r.SessionObserverToken == "" {
		t.Fatalf("result=%#v", r)
	}
	if !strings.Contains(f.evaluated, "email_otp_validate") || !strings.Contains(f.evaluated, "loader.js") {
		t.Fatalf("SDK script was not bound to requested flow: %s", f.evaluated)
	}
}
