package console

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

const authorizationPreferenceTestKey = "tosub2.authorization-method-v1"

type authorizationPreferenceStorage struct {
	values              map[string]string
	reads, writes       int
	failRead, failWrite bool
}

func authorizationPreferenceVM(t *testing.T, storage *authorizationPreferenceStorage) *sobek.Runtime {
	t.Helper()
	entry, err := filepath.Abs("../../web/src/lib/authorizationMethod.js")
	if err != nil {
		t.Fatal(err)
	}
	built := api.Build(api.BuildOptions{
		EntryPoints: []string{entry}, Bundle: true, Write: false,
		Format: api.FormatIIFE, GlobalName: "AuthorizationPreferences", Platform: api.PlatformNeutral,
	})
	if len(built.Errors) > 0 || len(built.OutputFiles) != 1 {
		t.Fatalf("authorization preference bundle failed: %v", built.Errors)
	}
	vm := sobek.New()
	if storage != nil {
		if storage.values == nil {
			storage.values = make(map[string]string)
		}
		err := vm.Set("window", map[string]any{"localStorage": map[string]any{
			"getItem": func(call sobek.FunctionCall) sobek.Value {
				storage.reads++
				if storage.failRead {
					panic(vm.NewGoError(errors.New("synthetic storage read failure")))
				}
				if value, ok := storage.values[call.Argument(0).String()]; ok {
					return vm.ToValue(value)
				}
				return sobek.Null()
			},
			"setItem": func(call sobek.FunctionCall) sobek.Value {
				storage.writes++
				if storage.failWrite {
					panic(vm.NewGoError(errors.New("synthetic storage write failure")))
				}
				storage.values[call.Argument(0).String()] = call.Argument(1).String()
				return sobek.Undefined()
			},
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := vm.RunString(string(built.OutputFiles[0].Contents)); err != nil {
		t.Fatal(err)
	}
	return vm
}

func authorizationPreferenceCall(t *testing.T, vm *sobek.Runtime, name string, args ...any) sobek.Value {
	t.Helper()
	fn, ok := sobek.AssertFunction(vm.Get("AuthorizationPreferences").ToObject(vm).Get(name))
	if !ok {
		t.Fatalf("missing %s export", name)
	}
	values := make([]sobek.Value, len(args))
	for i, arg := range args {
		values[i] = vm.ToValue(arg)
	}
	result, err := fn(sobek.Undefined(), values...)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func requireAuthorizationPreference(t *testing.T, vm *sobek.Runtime, action map[string]any, want string) {
	t.Helper()
	got := authorizationPreferenceCall(t, vm, "readAuthorizationMethodPreference", action).String()
	if got != want {
		t.Fatalf("authorization preference=%q, want %q for %+v", got, want, action)
	}
}

func TestAuthorizationPreferenceNormalizesAndLabelsSupportedMethods(t *testing.T) {
	vm := authorizationPreferenceVM(t, nil)
	key := vm.Get("AuthorizationPreferences").ToObject(vm).Get("AUTHORIZATION_METHOD_PREFERENCE_KEY").String()
	if key != authorizationPreferenceTestKey {
		t.Fatalf("unexpected preference storage key %q", key)
	}
	for _, value := range []any{"bps", "chatgpt", "BPS", " bps ", "unknown", "", nil, true, 1, map[string]any{"method": "bps"}} {
		want, label := "chatgpt", "ChatGPT"
		if value == "bps" {
			want, label = "bps", "BPS"
		}
		if got := authorizationPreferenceCall(t, vm, "normalizeAuthorizationMethod", value).String(); got != want {
			t.Errorf("normalize(%v)=%q, want %q", value, got, want)
		}
		if got := authorizationPreferenceCall(t, vm, "authorizationMethodLabel", value).String(); got != label {
			t.Errorf("label(%v)=%q, want %q", value, got, label)
		}
	}
}

func TestAuthorizationPreferencePersistsAcrossReopenedFlowsAndRefresh(t *testing.T) {
	storage := &authorizationPreferenceStorage{}
	vm := authorizationPreferenceVM(t, storage)
	if !authorizationPreferenceCall(t, vm, "saveAuthorizationMethodPreference", "bps").ToBoolean() {
		t.Fatal("BPS selection was not persisted")
	}
	if storage.values[authorizationPreferenceTestKey] != "bps" {
		t.Fatal("selection must be saved as a raw supported method")
	}
	// Closing or canceling an action causes no submission; opening either flow
	// must still recover the explicit selection from the same storage.
	for _, action := range []map[string]any{
		{"type": "reset_2fa", "authorizationMethod": "chatgpt"},
		{"type": "relogin", "authorizationMethod": "chatgpt"},
		{"type": "reauthorize"},
	} {
		requireAuthorizationPreference(t, vm, action, "bps")
	}
	// A fresh JavaScript runtime models reloads and newly opened same-origin tabs.
	refreshed := authorizationPreferenceVM(t, storage)
	requireAuthorizationPreference(t, refreshed, map[string]any{"authorizationMethod": "chatgpt"}, "bps")
	if !authorizationPreferenceCall(t, refreshed, "saveAuthorizationMethodPreference", "chatgpt").ToBoolean() {
		t.Fatal("ChatGPT selection could not replace BPS")
	}
	requireAuthorizationPreference(t, vm, map[string]any{"authorizationMethod": "bps"}, "chatgpt")
	if storage.writes != 2 || storage.values[authorizationPreferenceTestKey] != "chatgpt" {
		t.Fatalf("unexpected storage mutations: %+v", storage)
	}
}

func TestAuthorizationPreferenceFallbackPreservesAccountWithoutSavingIt(t *testing.T) {
	storage := &authorizationPreferenceStorage{}
	vm := authorizationPreferenceVM(t, storage)
	for _, method := range []string{"bps", "chatgpt", "unknown"} {
		want := "chatgpt"
		if method == "bps" {
			want = "bps"
		}
		requireAuthorizationPreference(t, vm, map[string]any{"authorizationMethod": method}, want)
	}
	if got := authorizationPreferenceCall(t, vm, "readAuthorizationMethodPreference").String(); got != "chatgpt" {
		t.Fatalf("empty action fallback=%q, want chatgpt", got)
	}
	if storage.writes != 0 || len(storage.values) != 0 {
		t.Fatalf("reading an account implicitly saved a global preference: %+v", storage)
	}
}

func TestAuthorizationPreferenceLockedRetryIgnoresAndPreservesSharedDefault(t *testing.T) {
	storage := &authorizationPreferenceStorage{values: map[string]string{authorizationPreferenceTestKey: "bps"}}
	vm := authorizationPreferenceVM(t, storage)
	for _, method := range []string{"bps", "chatgpt", "unknown"} {
		want := "chatgpt"
		if method == "bps" {
			want = "bps"
		}
		requireAuthorizationPreference(t, vm, map[string]any{"lockedMethod": true, "authorizationMethod": method}, want)
	}
	if storage.reads != 0 || storage.writes != 0 || storage.values[authorizationPreferenceTestKey] != "bps" {
		t.Fatalf("locked action accessed or changed shared preference: %+v", storage)
	}
}

func TestAuthorizationPreferenceInvalidStoredValuesUseUnsavedAccountFallback(t *testing.T) {
	storage := &authorizationPreferenceStorage{}
	vm := authorizationPreferenceVM(t, storage)
	for _, invalid := range []string{"", "BPS", " bps ", "unknown", `"bps"`, `{"authorizationMethod":"bps"}`, "[broken"} {
		storage.values[authorizationPreferenceTestKey] = invalid
		for _, method := range []string{"bps", "chatgpt"} {
			requireAuthorizationPreference(t, vm, map[string]any{"authorizationMethod": method}, method)
		}
		if storage.writes != 0 || storage.values[authorizationPreferenceTestKey] != invalid {
			t.Fatalf("read repaired storage or persisted a fallback for %q: %+v", invalid, storage)
		}
	}
}

func TestAuthorizationPreferenceRejectsUnsupportedSavesWithoutMutation(t *testing.T) {
	storage := &authorizationPreferenceStorage{values: map[string]string{authorizationPreferenceTestKey: "bps"}}
	vm := authorizationPreferenceVM(t, storage)
	for _, invalid := range []any{"", "BPS", " bps ", "unknown", nil, true, 1, map[string]any{"method": "chatgpt"}} {
		if authorizationPreferenceCall(t, vm, "saveAuthorizationMethodPreference", invalid).ToBoolean() {
			t.Errorf("invalid method %v was reported as saved", invalid)
		}
	}
	if authorizationPreferenceCall(t, vm, "saveAuthorizationMethodPreference").ToBoolean() {
		t.Fatal("missing selection was reported as saved")
	}
	if storage.writes != 0 || storage.values[authorizationPreferenceTestKey] != "bps" {
		t.Fatalf("invalid input replaced the existing preference: %+v", storage)
	}
}

func TestAuthorizationPreferenceStorageFailuresRemainUsable(t *testing.T) {
	storage := &authorizationPreferenceStorage{
		values:   map[string]string{authorizationPreferenceTestKey: "chatgpt"},
		failRead: true, failWrite: true,
	}
	vm := authorizationPreferenceVM(t, storage)
	requireAuthorizationPreference(t, vm, map[string]any{"authorizationMethod": "bps"}, "bps")
	if authorizationPreferenceCall(t, vm, "saveAuthorizationMethodPreference", "bps").ToBoolean() {
		t.Fatal("failed storage write was reported as saved")
	}
	if storage.writes != 1 || storage.values[authorizationPreferenceTestKey] != "chatgpt" {
		t.Fatalf("storage failure changed the saved selection: %+v", storage)
	}
	for _, setup := range []string{
		"delete window",
		"window = {}",
		"window = { localStorage: null }",
		"window = {}; Object.defineProperty(window, 'localStorage', { get() { throw new Error('synthetic denied storage'); } });",
	} {
		if _, err := vm.RunString(setup); err != nil {
			t.Fatal(err)
		}
		requireAuthorizationPreference(t, vm, map[string]any{"authorizationMethod": "bps"}, "bps")
		if authorizationPreferenceCall(t, vm, "saveAuthorizationMethodPreference", "bps").ToBoolean() {
			t.Fatalf("unavailable storage was reported as saved after %s", setup)
		}
	}
}
