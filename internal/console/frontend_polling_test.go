package console

import (
	"path/filepath"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/grafana/sobek"
)

func frontendPollingVM(t *testing.T) *sobek.Runtime {
	t.Helper()
	entry, err := filepath.Abs("../../web/src/lib/polling.js")
	if err != nil {
		t.Fatal(err)
	}
	built := api.Build(api.BuildOptions{EntryPoints: []string{entry}, Bundle: true, Write: false, Format: api.FormatIIFE, GlobalName: "Polling", Platform: api.PlatformNeutral})
	if len(built.Errors) != 0 || len(built.OutputFiles) != 1 {
		t.Fatalf("polling bundle: %v", built.Errors)
	}
	vm := sobek.New()
	frontendPollingRun(t, vm, string(built.OutputFiles[0].Contents))
	frontendPollingRun(t, vm, `
var timers = new Map(), nextTimer = 0, events = new Map(), requests = [], results = [], errors = [], starts = 0;
function assert(value, message) { if (!value) throw new Error(message); }
function target(prefix) { return {
  addEventListener(name, fn) { events.set(prefix + name, fn); },
  removeEventListener(name, fn) { if (events.get(prefix + name) === fn) events.delete(prefix + name); }
}; }
var environment = {
  window: Object.assign(target("window:"), {
    setTimeout(fn, delay) { const id = ++nextTimer; timers.set(id, {fn, delay}); return id; },
    clearTimeout(id) { timers.delete(id); }
  }),
  document: Object.assign(target("document:"), {hidden: false}), navigator: {onLine: true},
  AbortController: class { constructor() { this.signal = {aborted: false}; } abort() { this.signal.aborted = true; } }
};
function fire(delay) {
  const entry = [...timers].find(([, value]) => value.delay === delay);
  assert(entry, "missing timer " + delay + ": " + JSON.stringify([...timers.values()].map(value => value.delay)));
  timers.delete(entry[0]); entry[1].fn();
}
function event(name) { const fn = events.get(name); assert(fn, "missing event " + name); fn(); }
var polling = Polling.createPollingTask({
  environment, intervalMs: 100, timeoutMs: 1000, maxIntervalMs: 800,
  request(signal) { return new Promise((resolve, reject) => requests.push({signal, resolve, reject})); },
  onData(data) { results.push(data); }, onError(error) { errors.push(error.message); }, onStart() { starts++; }
});
`)
	return vm
}

func frontendPollingRun(t *testing.T, vm *sobek.Runtime, script string) {
	t.Helper()
	if _, err := vm.RunString(script); err != nil {
		t.Fatal(err)
	}
}

func TestFrontendPollingSerializesRefreshAndReleasesResources(t *testing.T) {
	vm := frontendPollingVM(t)
	frontendPollingRun(t, vm, `assert(requests.length === 1, "initial read"); polling.refresh(); polling.refresh(); assert(requests.length === 1, "overlapping reads"); requests[0].resolve("first");`)
	frontendPollingRun(t, vm, `assert(results.join() === "first", "missing response"); fire(0);`)
	frontendPollingRun(t, vm, `assert(requests.length === 2, "refresh was not coalesced"); requests[1].resolve("second");`)
	frontendPollingRun(t, vm, `assert(timers.size === 1, "leaked request timers"); fire(100);`)
	frontendPollingRun(t, vm, `assert(requests.length === 3, "normal interval not resumed"); polling.stop(); assert(requests[2].signal.aborted, "stop did not abort"); assert(timers.size === 0 && events.size === 0, "stop leaked resources"); requests[2].resolve("late");`)
	frontendPollingRun(t, vm, `assert(results.join() === "first,second" && errors.length === 0, "stopped read updated state");`)
}

func TestFrontendPollingTimeoutDiscardsLateResponseAndRecovers(t *testing.T) {
	vm := frontendPollingVM(t)
	frontendPollingRun(t, vm, `fire(1000);`)
	frontendPollingRun(t, vm, `assert(requests[0].signal.aborted && errors.length === 1, "timeout was not reported"); fire(200);`)
	frontendPollingRun(t, vm, `requests[1].resolve("current"); requests[0].resolve("obsolete");`)
	frontendPollingRun(t, vm, `assert(results.join() === "current", "late result replaced current state"); fire(100);`)
	frontendPollingRun(t, vm, `assert(requests.length === 3, "success did not restore regular interval"); polling.stop();`)
}

func TestFrontendPollingPausesHiddenAndOfflineAndResumes(t *testing.T) {
	vm := frontendPollingVM(t)
	frontendPollingRun(t, vm, `environment.document.hidden = true; event("document:visibilitychange"); assert(requests[0].signal.aborted && timers.size === 0, "hidden read continued"); requests[0].resolve("hidden");`)
	frontendPollingRun(t, vm, `assert(results.length === 0 && errors.length === 0, "hidden response leaked"); environment.document.hidden = false; event("document:visibilitychange");`)
	frontendPollingRun(t, vm, `assert(requests.length === 2, "visible tab did not refresh"); environment.navigator.onLine = false; event("window:offline"); assert(requests[1].signal.aborted && errors.length === 1 && timers.size === 0, "offline state not reported"); polling.refresh();`)
	frontendPollingRun(t, vm, `assert(requests.length === 2, "offline request dispatched"); environment.navigator.onLine = true; event("window:online");`)
	frontendPollingRun(t, vm, `assert(requests.length === 3, "network recovery did not refresh"); requests[2].resolve("online");`)
	frontendPollingRun(t, vm, `assert(results.join() === "online", "network recovery missing"); polling.stop();`)
}

func TestFrontendPollingDoesNotRetryBusinessRefusal(t *testing.T) {
	vm := frontendPollingVM(t)
	frontendPollingRun(t, vm, `const denied = new Error("synthetic access refusal"); denied.status = 403; requests[0].reject(denied);`)
	frontendPollingRun(t, vm, `assert(errors.length === 1 && timers.size === 0, "business refusal entered a network retry loop"); polling.refresh();`)
	frontendPollingRun(t, vm, `assert(requests.length === 2, "intentional refresh was blocked"); const limited = new Error("synthetic rate limit"); limited.status = 429; requests[1].reject(limited);`)
	frontendPollingRun(t, vm, `fire(200);`)
	frontendPollingRun(t, vm, `assert(requests.length === 3, "transient rate limit did not retry"); polling.stop();`)
}

func TestFrontendPollingBoundsBackoffAndDoesNotSwallowRepeatedFailures(t *testing.T) {
	vm := frontendPollingVM(t)
	for i, delay := range []int{200, 400, 800, 800} {
		frontendPollingRun(t, vm, `requests[requests.length - 1].reject(new Error("synthetic failure"));`)
		if got := vm.Get("errors").ToObject(vm).Get("length").ToInteger(); got != int64(i+1) {
			t.Fatalf("missing settled failure: %d", got)
		}
		fn, _ := sobek.AssertFunction(vm.Get("fire"))
		if _, err := fn(sobek.Undefined(), vm.ToValue(delay)); err != nil {
			t.Fatal(err)
		}
	}
	frontendPollingRun(t, vm, `polling.stop();`)
}
