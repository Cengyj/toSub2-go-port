package config

import "testing"

func TestGoAuthenticationConfiguration(t *testing.T) {
	t.Setenv("TOSUB2_AUTH_TLS_PROFILE", "")
	cfg, err := Parse(nil)
	if err != nil || cfg.AuthTLSProfile != "chrome146" {
		t.Fatal("Go defaults failed", err)
	}
	t.Setenv("TOSUB2_AUTH_TLS_PROFILE", "chrome145")
	cfg, err = Parse([]string{"--auth-tls-profile=chrome146"})
	if err != nil || cfg.AuthTLSProfile != "chrome146" {
		t.Fatal("Go profile override failed", err)
	}
	cfg, err = Parse([]string{"--email=fixture@example.test", "--native-http"})
	if err != nil || !cfg.CLI.NativeHTTP {
		t.Fatal("Go native diagnostics rejected", err)
	}
}

func TestRemovedRuntimeOptionsRejected(t *testing.T) {
	for _, argument := range []string{"--auth-transport=legacy", "--auth-transport=browser", "--legacy-js-runtime=go", "--legacy-js-runtime=node", "--legacy-tls-profile=chrome146", "--browser=false", "--browser=true", "--chrome-path=/fixture/chrome", "--legacy-node=/fixture/node", "--legacy-runtime-dir=/fixture/runtime", "--legacy-python=/fixture/python", "--legacy-worker=/fixture/worker.py"} {
		if _, err := Parse([]string{argument}); err == nil {
			t.Fatal("removed runtime option accepted", argument)
		}
	}
}
