package console

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"testing"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/store"
)

func TestFrontendAccountPollingRejectsInvalidSnapshotBeforePublishing(t *testing.T) {
	for _, payload := range []string{
		`{}`, `[]`, `{"jobs":{}}`, `{"jobs":[null]}`, `{"jobs":[{"id":"new"}]}`,
		`{"jobs":[],"selection":{}}`, `{"jobs":[],"selection":[null]}`,
		`{"jobs":[],"pagination":{"page":1}}`,
		`{"jobs":[],"stats":{"active":{},"queued":0,"completed":0}}`,
		`{"jobs":[{"id":"same","email":"one@example.invalid"},{"id":"same","email":"two@example.invalid"}]}`,
	} {
		t.Run(payload, func(t *testing.T) {
			vm := frontendInsightsVM(t, "hooks/serverState.js")
			if err := vm.Set("invalidJSON", payload); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
var presented={}, published=0;
function publish(key){return value=>{presented[key]=value;published++;};}
var props={token:"synthetic",page:1,emailFilter:[],setJobs:publish("jobs"),setJobSelectionIndex:publish("selection"),setPagination:publish("pagination"),setStats:publish("stats"),setPage:publish("page"),setConnected:value=>{presented.connected=value;},setConnectionError:value=>{presented.error=value;}};
function renderAccounts(){hookCursor=0;const refresh=Insights.useAccountPolling(props);for(const run of effectQueue.splice(0))run();return refresh;}
var refresh=renderAccounts();
`)
			frontendPollingRun(t, vm, `ok(requests[0],{jobs:[{id:"saved",email:"saved@example.invalid",status:"completed"}],stats:{active:0,queued:0,completed:1}});`)
			frontendPollingRun(t, vm, `var before=JSON.stringify(presented), count=published;refresh();`)
			frontendPollingRun(t, vm, `const malformed=JSON.parse(invalidJSON);if(Array.isArray(malformed))malformed.jobs=[];ok(requests[1],malformed);`)
			frontendPollingRun(t, vm, `
assert(published===count,"invalid response published state before validation");
const previous=JSON.parse(before);for(const key of ["jobs","selection","pagination","stats"])assert(JSON.stringify(presented[key])===JSON.stringify(previous[key]),"invalid response destroyed last valid "+key);
assert(!presented.connected&&presented.error==="账号列表响应不完整","invalid snapshot was silent, exposed response contents, or duplicated the global notice");
refresh();
`)
			frontendPollingRun(t, vm, `ok(requests[2],{jobs:[],selection:[],pagination:{page:1,pageSize:20,total:0,totalPages:1},stats:{active:0,queued:0,completed:0}});`)
			frontendPollingRun(t, vm, `assert(presented.connected&&!presented.error&&presented.jobs.length===0,"valid empty list failed to recover");unmount();assert(timers.size===0,"account read cleanup leaked timers");`)
		})
	}
}

func TestFrontendAccountPollingAcceptsEmptyEmailHTTPProjection(t *testing.T) {
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	ctx := context.Background()
	root, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vault := credentials.NewMemoryStore()
	original := jobs.NewManager(ctx, 1, root, nil)
	original.SetCredentialStore(vault)
	if _, err := original.Submit(jobs.Spec{ID: "legacy-empty-email", ImportOnly: true}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.Config{OutputRoot: root.Root, StartupToken: "synthetic"}, original, embed.FS{}, nil, vault)
	t.Cleanup(server.Close)
	response := frontendRequest(server, http.MethodGet, "/api/jobs?page=1", nil, "synthetic")
	if response.Code != http.StatusOK {
		t.Fatalf("jobs projection returned %d", response.Code)
	}
	var projection map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"jobs", "selection"} {
		var rows []map[string]any
		if err := json.Unmarshal(projection[key], &rows); err != nil || len(rows) != 1 || rows[0]["email"] != "" {
			t.Fatalf("production %s does not explicitly project the empty email: %v", key, err)
		}
	}
	vm := frontendInsightsVM(t, "hooks/serverState.js")
	if err := vm.Set("projectionJSON", response.Body.String()); err != nil {
		t.Fatal(err)
	}
	frontendPollingRun(t, vm, `
var published={},error="",connected=false;
const props={token:"synthetic",page:1,emailFilter:[],setJobs:value=>{published.jobs=value;},setJobSelectionIndex:value=>{published.selection=value;},setPagination:value=>{published.pagination=value;},setStats:value=>{published.stats=value;},setConnected:value=>{connected=value;},setPage(){},setConnectionError:value=>{error=value;}};
hookCursor=0;Insights.useAccountPolling(props);for(const run of effectQueue.splice(0))run();
`)
	frontendPollingRun(t, vm, `ok(requests[0],JSON.parse(projectionJSON));`)
	frontendPollingRun(t, vm, `
assert(connected&&!error&&published.jobs.length===1&&published.selection.length===1,"valid empty-email HTTP snapshot was rejected by the production polling hook");
assert(published.jobs[0].id==="legacy-empty-email"&&published.jobs[0].email==="","legacy identity was dropped or fabricated");
assert(published.pagination.total===1&&published.stats.queued===0,"import changed count or entered the authorization queue");
unmount();assert(timers.size===0,"account read leaked timers");
`)
}

func TestFrontendRestoredHTTPProjectionRequiresKnownEmailIdentity(t *testing.T) {
	t.Setenv("SUB2API_BASE_URL", "")
	t.Setenv("SUB2API_ADMIN_API_KEY", "")
	ctx := context.Background()
	root, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteJSON(ctx, "job-meta.json", map[string]any{"jobs": []any{
		map[string]any{"id": "missing-email", "status": "imported"},
		map[string]any{"id": "empty-email", "email": "", "status": "imported"},
		map[string]any{"id": "valid-email", "email": "synthetic@example.invalid", "status": "imported"},
	}}); err != nil {
		t.Fatal(err)
	}
	vault := credentials.NewMemoryStore()
	manager := jobs.NewManager(ctx, 1, root, nil)
	manager.SetCredentialStore(vault)
	if err := manager.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	server := NewServer(config.Config{OutputRoot: root.Root, StartupToken: "synthetic"}, manager, embed.FS{}, nil, vault)
	t.Cleanup(server.Close)
	response := frontendRequest(server, http.MethodGet, "/api/jobs?page=1", nil, "synthetic")
	if response.Code != http.StatusOK {
		t.Fatalf("restored jobs projection returned %d", response.Code)
	}
	var projection map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"jobs", "selection"} {
		var rows []map[string]any
		if err := json.Unmarshal(projection[key], &rows); err != nil || len(rows) != 1 || rows[0]["id"] != "valid-email" || rows[0]["email"] != "synthetic@example.invalid" {
			t.Fatalf("restored %s must retain only known email identity: %v", key, err)
		}
	}
}

func TestFrontendMonitorPollingDiscardsObsoleteReadFailure(t *testing.T) {
	for _, outcome := range []string{"rejected", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			vm := frontendInsightsVM(t, "hooks/serverState.js")
			if err := vm.Set("outcome", outcome); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `
var version={current:0},status={configured:true,enabled:true},settings={};
var props={token:"synthetic",enabled:true,version,setStatus:value=>{status=value;},setSettings:update=>{settings=update(settings);}};
function renderMonitor(){hookCursor=0;const error=Insights.useMonitorPolling(props);for(const run of effectQueue.splice(0))run();return error;}
renderMonitor();
`)
			frontendPollingRun(t, vm, `version.current++;status={configured:true,enabled:false};if(outcome==="timeout")fire(10000);else requests[0].reject(new Error("obsolete read failure"));`)
			frontendPollingRun(t, vm, `assert(renderMonitor()===""&&!status.enabled,"old monitor failure overwrote confirmed saved configuration state");fire(10000);`)
			frontendPollingRun(t, vm, `requests[1].reject(new Error("current read failure"));`)
			frontendPollingRun(t, vm, `assert(renderMonitor().includes("网络连接中断"),"current monitor error was swallowed with obsolete errors");unmount();assert(timers.size===0,"monitor cleanup leaked timers");`)
		})
	}
}

func TestFrontendInsightSettingsSaveDiscardsObsoletePollFailure(t *testing.T) {
	for _, outcome := range []string{"rejected", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			vm := frontendInsightsVM(t, "hooks/accountInsights.js")
			if err := vm.Set("outcome", outcome); err != nil {
				t.Fatal(err)
			}
			frontendPollingRun(t, vm, `options={...options,active:false};var hook=renderHook(options);`)
			frontendPollingRun(t, vm, `ok(requests[0],config);`)
			frontendPollingRun(t, vm, `hook=renderHook(options);fire(30000);`)
			frontendPollingRun(t, vm, `hook.setDraft({...hook.draft,intervalMinutes:30});hook=renderHook(options);hook.saveSettings();`)
			frontendPollingRun(t, vm, `assert(requests[2].options.method==="PUT","fixture did not exercise a real settings save");ok(requests[2],{...config,revision:2,intervalMinutes:30});`)
			frontendPollingRun(t, vm, `if(outcome==="timeout")fire(10000);else requests[1].reject(new Error("obsolete settings read"));`)
			frontendPollingRun(t, vm, `hook=renderHook(options);assert(!hook.settingsError&&!hook.uncertain&&hook.settings.revision===2&&hook.draft.intervalMinutes===30,"obsolete settings failure replaced confirmed save or changed its draft");fire(30000);`)
			frontendPollingRun(t, vm, `requests[3].reject(new Error("current settings failure"));`)
			frontendPollingRun(t, vm, `hook=renderHook(options);assert(hook.settingsError.includes("网络连接中断"),"current settings read failure was hidden");unmount();assert(timers.size===0,"settings cleanup leaked timers");`)
		})
	}
}
