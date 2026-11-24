package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

// These are command inputs, not a copy of the row presentation schema. In
// particular, false flags and the operation generation must survive compaction.
var compactSelectionFields = []string{
	"id", "email", "createdAt", "lastOperationAt", "attempt", "authorizationMethod", "status", "operationStatus",
	"canResume", "canDownload", "canRegenerate", "canRetry", "canForceRelogin", "canSetupTotp", "canResetTotp", "canAddPassword", "canCompleteTotp",
	"totpExportBlocked", "totpRecoveryPending", "totpCredentialInvalidated", "totpCompletionPending", "totpWorkflowStatus",
}

func readAccountSelectionPage(t *testing.T, s *Server, method, path string, body any) map[string]any {
	t.Helper()
	w := frontendRequest(s, method, path, body, "test")
	if w.Code != http.StatusOK {
		t.Fatalf("account page failed: %s %d %s", path, w.Code, w.Body.String())
	}
	var page map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func accountSelectionRows(t *testing.T, page map[string]any, key string) []map[string]any {
	t.Helper()
	values, ok := page[key].([]any)
	if !ok {
		t.Fatalf("%s must be an array, including when empty", key)
	}
	rows := make([]map[string]any, 0, len(values))
	for _, value := range values {
		row, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%s contains a non-object", key)
		}
		rows = append(rows, row)
	}
	return rows
}

func assertCompactAccountSelection(t *testing.T, full, compact map[string]any) map[string]map[string]any {
	t.Helper()
	if _, present := full["selectionMode"]; present || compact["selectionMode"] != "compact" {
		t.Fatal("compact must be explicitly identified while the default response stays compatible")
	}
	for key, want := range full {
		if key != "selection" && !reflect.DeepEqual(compact[key], want) {
			t.Fatalf("compaction changed page field %s", key)
		}
	}
	if len(compact) != len(full)+1 {
		t.Fatal("compaction changed unrelated response fields")
	}
	want := accountSelectionRows(t, full, "selection")
	got := accountSelectionRows(t, compact, "selection")
	if len(got) != len(want) {
		t.Fatalf("cross-page selection was truncated: %d, want %d", len(got), len(want))
	}
	indexed := make(map[string]map[string]any, len(got))
	for i, row := range got {
		if len(row) != len(compactSelectionFields) {
			t.Fatalf("compact row has %d fields, want %d", len(row), len(compactSelectionFields))
		}
		for _, field := range compactSelectionFields {
			value, present := row[field]
			if !present || value == nil || !reflect.DeepEqual(value, want[i][field]) {
				t.Fatalf("compact field %s differs from the final public capability", field)
			}
		}
		id, ok := row["id"].(string)
		if !ok || id == "" || indexed[id] != nil {
			t.Fatal("compact index lost unique account identity")
		}
		indexed[id] = row
	}
	return indexed
}

func TestAccountSelectionCompactPreservesDefaultContractAndCompleteRows(t *testing.T) {
	s, m := newMonitorTestServer(t)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	privateValues := []string{"Selection-password9!", resetAPISeed, "selection-proxy-password", "selection-mail-token", "selection-admin-key"}
	for _, spec := range []jobs.Spec{
		{ID: "plain", Email: "plain@example.test", ImportOnly: true},
		{ID: "private", Email: "private@example.test", ImportOnly: true, Input: map[string]any{
			"password": privateValues[0], "totpSecret": privateValues[1], "proxyUrl": "http://user:" + privateValues[2] + "@proxy.example.test:8080",
			"mailApiUrl": "https://mail.example.test/?token=" + privateValues[3],
		}},
		{ID: "bps", Email: "bps@example.test", ImportOnly: true, Input: map[string]any{"authorizationMethod": "bps"}},
		{ID: "saved", Email: "saved@example.test", ImportOnly: true},
		{ID: "queued", Email: "queued@example.test"},
		{ID: "canceled", Email: "canceled@example.test"},
		{ID: "workflow", Email: "workflow@example.test", ImportOnly: true, Input: map[string]any{"totpSecret": resetAPISeed}},
	} {
		if _, err := m.Submit(spec); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.CancelWithError("canceled"); err != nil {
		t.Fatal(err)
	}
	if err := writeFlowExport(s.cfg.OutputRoot, "saved", "saved@example.test"); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult("saved", true, "synthetic saved authorization")
	m.Emit(jobs.JobEvent{JobID: "saved", Status: jobs.StatusSucceeded})
	m.Emit(jobs.JobEvent{JobID: "private", Prompt: strings.Join(privateValues[:4], " "), Data: map[string]any{"phoneError": strings.Join(privateValues[:4], " ")}})
	if _, err := m.QueueOperation("workflow", "reset-2fa", map[string]any{
		"reloginAfterTotp": true, "uploadAfterTotp": true,
		"sub2apiConfig": map[string]any{"baseUrl": "https://selection-upload.example.test", "adminApiKey": privateValues[4]},
	}); err != nil {
		t.Fatal(err)
	}
	// Integration credentials belong to an explicitly selected workflow. A
	// plain import intentionally clears them before any operation can start.
	m.Emit(jobs.JobEvent{JobID: "workflow", Prompt: privateValues[4], Data: map[string]any{"phoneError": privateValues[4]}})
	full := readAccountSelectionPage(t, s, http.MethodGet, "/api/jobs?page=2&pageSize=2", nil)
	compact := readAccountSelectionPage(t, s, http.MethodGet, "/api/jobs?selection=compact&page=2&pageSize=2", nil)
	index := assertCompactAccountSelection(t, full, compact)
	if len(accountSelectionRows(t, compact, "jobs")) != 2 || len(index) != 7 {
		t.Fatal("page size changed the complete selection index")
	}
	if index["plain"]["canAddPassword"] != true || index["plain"]["canSetupTotp"] != true || index["private"]["canResetTotp"] != true || index["saved"]["canRegenerate"] != true || index["canceled"]["canRetry"] != true || index["bps"]["authorizationMethod"] != "bps" || index["queued"]["canForceRelogin"] != false || index["workflow"]["totpWorkflowStatus"] != "pending" || index["workflow"]["totpExportBlocked"] != true {
		t.Fatal("fixture did not retain the distinct final command capabilities")
	}
	// Every default selection item still has the complete existing row contract
	// apart from its four historically omitted fields, even outside the page.
	all := readAccountSelectionPage(t, s, http.MethodGet, "/api/jobs?pageSize=100", nil)
	allRows := accountSelectionRows(t, all, "jobs")
	defaultSelection := accountSelectionRows(t, full, "selection")
	for i, row := range allRows {
		for _, field := range []string{"prompt", "updatedAt", "lastError", "queuePosition", "accountRequestObservations", "canRefreshSubscription", "totpCompletionHint"} {
			if _, present := row[field]; !present {
				t.Fatalf("full row lost existing field %s", field)
			}
		}
		for _, field := range []string{"prompt", "updatedAt", "lastError", "logs"} {
			delete(row, field)
		}
		if !reflect.DeepEqual(row, defaultSelection[i]) {
			t.Fatal("default selection changed its existing full contract")
		}
	}
	for _, page := range []map[string]any{full, compact} {
		raw, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		for index, private := range append(privateValues, "synthetic-access", "synthetic-refresh", "synthetic-id") {
			if bytes.Contains(raw, []byte(private)) {
				t.Fatalf("account page exposed synthetic private value %d", index)
			}
		}
	}
}

func TestAccountSelectionCompactFilteringKeepsGlobalStatsAndEmptyArrays(t *testing.T) {
	s, m := newMonitorTestServer(t)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	for _, spec := range []jobs.Spec{
		{ID: "older", Email: " SAME@example.test ", ImportOnly: true},
		{ID: "latest", Email: "same@example.test", ImportOnly: true},
		{ID: "other", Email: "other@example.test", ImportOnly: true},
		{ID: "queued", Email: "queued@example.test"},
	} {
		if _, err := m.Submit(spec); err != nil {
			t.Fatal(err)
		}
	}
	full := readAccountSelectionPage(t, s, http.MethodGet, "/api/jobs?pageSize=1", nil)
	for _, tc := range []struct {
		name, method, path string
		body               any
		matched            int
	}{
		{"get", http.MethodGet, "/api/jobs?selection=compact&email=other@example.test,SAME@example.test&page=99&pageSize=1", nil, 2},
		{"post", http.MethodPost, "/api/jobs/query?selection=compact", map[string]any{"emails": []string{" SAME@example.test ", "other@example.test", "same@example.test"}, "page": 99, "pageSize": 1}, 2},
		{"empty-get", http.MethodGet, "/api/jobs?selection=compact&email=absent@example.test&page=99", nil, 0},
		{"empty-post", http.MethodPost, "/api/jobs/query?selection=compact", map[string]any{"emails": []string{"absent@example.test"}, "page": 99}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defaultPath := strings.Replace(strings.Replace(tc.path, "selection=compact&", "", 1), "?selection=compact", "", 1)
			defaultPage := readAccountSelectionPage(t, s, tc.method, defaultPath, tc.body)
			page := readAccountSelectionPage(t, s, tc.method, tc.path, tc.body)
			assertCompactAccountSelection(t, defaultPage, page)
			selection := accountSelectionRows(t, page, "selection")
			rows := accountSelectionRows(t, page, "jobs")
			pagination := page["pagination"].(map[string]any)
			if page["selectionMode"] != "compact" || len(selection) != tc.matched || page["total"] != float64(tc.matched) || pagination["totalAll"] != float64(3) || !reflect.DeepEqual(page["stats"], full["stats"]) || full["stats"].(map[string]any)["queued"] != float64(1) {
				t.Fatal("filtered list changed global statistics or cross-page scope")
			}
			if tc.matched == 0 {
				if len(rows) != 0 || page["page"] != float64(1) || page["pages"] != float64(1) {
					t.Fatal("empty result did not preserve empty arrays and clamped page")
				}
			} else if len(rows) != 1 || page["page"] != float64(2) {
				t.Fatal("filtered pagination was not clamped to the last matching page")
			}
			for _, row := range selection {
				if row["id"] != "latest" && row["id"] != "other" {
					t.Fatal("filtered selection retained old email history or an excluded account")
				}
			}
		})
	}
	if len(m.List()) != 4 {
		t.Fatal("latest-per-email selection discarded retained account history")
	}
}

func TestAccountSelectionCompactRejectsInvalidModeBeforeDiscovery(t *testing.T) {
	s, m := newMonitorTestServer(t)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	if err := writeFlowExport(s.cfg.OutputRoot, "incoming", "incoming@example.test"); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, query := range []string{"selection", "selection=", "selection=full", "selection=COMPACT", "selection=%20compact", "selection=compact&selection=compact", "selection=compact&selection=full"} {
			t.Run(method+"/"+query, func(t *testing.T) {
				path := "/api/jobs?" + query
				var body any
				if method == http.MethodPost {
					path, body = "/api/jobs/query?"+query, map[string]any{"emails": []string{"incoming@example.test"}}
				}
				w := frontendRequest(s, method, path, body, "test")
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "selection") || len(m.List()) != 0 {
					t.Fatal("invalid selection mode was accepted or performed discovery", w.Code)
				}
			})
		}
	}
	page := readAccountSelectionPage(t, s, http.MethodGet, "/api/jobs?selection=compact", nil)
	if len(accountSelectionRows(t, page, "selection")) != 1 {
		t.Fatal("valid list did not discover the synthetic output after invalid requests")
	}
	for _, emails := range [][]string{nil, {"invalid-email"}, make([]string, 501)} {
		for i := range emails {
			if emails[i] == "" {
				emails[i] = fmt.Sprintf("account-%d@example.test", i)
			}
		}
		w := frontendRequest(s, http.MethodPost, "/api/jobs/query?selection=compact", map[string]any{"emails": emails}, "test")
		if w.Code != http.StatusBadRequest {
			t.Fatal("compact option weakened the email-query contract", w.Code)
		}
	}
}

func TestAccountSelectionCompactRechecksNewHistoricalJournalAndCommandGate(t *testing.T) {
	s, m := newMonitorTestServer(t)
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	owner := importTOTPActionFixture(t, m, "old-journal-owner", true)
	peer, err := m.Submit(jobs.Spec{ID: "latest-journal-peer", Email: owner.Email, ImportOnly: true, Input: map[string]any{"totpSecret": resetAPISeed}})
	if err != nil {
		t.Fatal(err)
	}
	importTOTPActionFixture(t, m, "visible-other-account", false)
	if err := writeFlowExport(s.cfg.OutputRoot, peer.ID, peer.Email); err != nil {
		t.Fatal(err)
	}
	m.UpdateResult(peer.ID, true, "synthetic saved authorization")
	read := func() map[string]map[string]any {
		t.Helper()
		compact := readAccountSelectionPage(t, s, http.MethodGet, "/api/jobs?selection=compact&pageSize=1", nil)
		full := readAccountSelectionPage(t, s, http.MethodGet, "/api/jobs?pageSize=1", nil)
		if accountSelectionRows(t, compact, "jobs")[0]["id"] == peer.ID {
			t.Fatal("fixture must exercise a selected account outside the visible page")
		}
		return assertCompactAccountSelection(t, full, compact)
	}
	before := read()[peer.ID]
	if before["canDownload"] != true || before["totpExportBlocked"] != false || before["canRetry"] != true || before["canResetTotp"] != true {
		t.Fatal("healthy synthetic authorization did not start eligible")
	}
	path := filepath.Join(s.cfg.OutputRoot, owner.ID, "totp-setup-result.json")
	if err := protocol.WritePrivateJSON(path, map[string]any{"version": 1, "email": owner.Email, "synthetic_unknown_record": true, "secret": "selection-private-journal-seed", "receipt": "selection-private-receipt"}); err != nil {
		t.Fatal(err)
	}
	evidence, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// No sleep, metadata mutation or force-refresh: external recovery evidence
	// must be visible on the very next compact request, including older history.
	blocked := read()[peer.ID]
	for _, field := range []string{"canDownload", "canRetry", "canResume", "canRegenerate", "canForceRelogin", "canSetupTotp", "canResetTotp", "canAddPassword", "canCompleteTotp"} {
		if blocked[field] != false {
			t.Fatalf("new journal left stale compact capability %s", field)
		}
	}
	if blocked["totpExportBlocked"] != true || blocked["totpCompletionPending"] != false || blocked["lastOperationAt"] != before["lastOperationAt"] {
		t.Fatal("journal changed operation identity or failed to quarantine the account")
	}
	raw, err := json.Marshal(readAccountSelectionPage(t, s, http.MethodGet, "/api/jobs?selection=compact&pageSize=1", nil))
	if err != nil || bytes.Contains(raw, []byte("selection-private-journal-seed")) || bytes.Contains(raw, []byte("selection-private-receipt")) {
		t.Fatal("journal evidence leaked into list projection", err)
	}
	assertTOTPAuthorizationExportsBlocked(t, s, m, peer)
	w := frontendRequest(s, http.MethodPost, "/api/jobs/"+peer.ID+"/setup-2fa", map[string]any{"resetTotp": true}, "test")
	if w.Code != http.StatusConflict || m.QueuePosition(peer.ID) != 0 || m.QueuePosition(owner.ID) != 0 {
		t.Fatal("an old eligible list result bypassed the execution gate", w.Code)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, evidence) {
		t.Fatal("rejected command changed private recovery evidence", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if restored := read()[peer.ID]; restored["totpExportBlocked"] != false || restored["canDownload"] != true {
		t.Fatal("a prior compact result cached removed synthetic recovery evidence")
	}
}

func TestAccountSelectionCompactKeepsBoundLocalCompletionDistinctFromUnknown(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("activation-confirmed-%t", confirmed), func(t *testing.T) {
			var s *Server
			var m *jobs.Manager
			var job *jobs.Job
			if confirmed {
				s, m, job = completionAPIFixture(t, "business_receipt")
			} else {
				s, m, job = recoveryAPIFixture(t)
			}
			t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
			importTOTPActionFixture(t, m, "visible-completion-peer", false)
			full := readAccountSelectionPage(t, s, http.MethodGet, "/api/jobs?pageSize=1", nil)
			compact := readAccountSelectionPage(t, s, http.MethodGet, "/api/jobs?selection=compact&pageSize=1", nil)
			row := assertCompactAccountSelection(t, full, compact)[job.ID]
			if accountSelectionRows(t, compact, "jobs")[0]["id"] == job.ID || row["canCompleteTotp"] != confirmed || row["totpCompletionPending"] != confirmed || row["canResetTotp"] != false || row["canDownload"] != false || row["totpExportBlocked"] != true {
				t.Fatal("compact index confused a local completion with an unknown remote result")
			}
			for _, field := range []string{"createdAt", "lastOperationAt"} {
				if _, err := time.Parse(time.RFC3339Nano, row[field].(string)); err != nil {
					t.Fatal("compact confirmation generation is not a serialized timestamp", err)
				}
			}
			raw, err := json.Marshal(compact)
			if err != nil || bytes.Contains(raw, []byte(resetAPISeed)) || bytes.Contains(raw, []byte(completedResetSeed)) {
				t.Fatal("compact completion exposed a seed", err)
			}
			response := frontendRequest(s, http.MethodPost, "/api/jobs/"+job.ID+"/complete-2fa", map[string]any{}, "test")
			if confirmed {
				queued, _ := m.Get(job.ID)
				if response.Code != http.StatusOK || queued.Operation != "complete-2fa" || m.QueuePosition(job.ID) != 1 {
					t.Fatal("eligible off-page local completion could not be submitted")
				}
			} else if response.Code != http.StatusConflict || m.QueuePosition(job.ID) != 0 {
				t.Fatal("unknown activation acquired local completion authority")
			}
		})
	}
}
