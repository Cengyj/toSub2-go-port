package jobs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tosub2-go-port/internal/store"
)

func writePlanFixture(t *testing.T, root, id, email, plan string) {
	t.Helper()
	dir, err := store.New(filepath.Join(root, id))
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": plan}})
	token := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
	if err := dir.WriteJSON(context.Background(), "sub2api-import-oauth.json", map[string]any{
		"type": "sub2api-data", "accounts": []any{map[string]any{"credentials": map[string]any{"email": email, "id_token": token, "access_token": "private-access-fixture"}}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreIdentifiesPlanWithoutLoggingIn(t *testing.T) {
	ctx := context.Background()
	st, _ := store.New(t.TempDir())
	writePlanFixture(t, st.Root, "saved", "fixture@example.test", "self_serve_business_prolite")
	m := NewManager(ctx, 1, st, nil)
	if err := m.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	job, ok := m.Get("saved")
	if !ok || job.PlanType != "business_premium" || job.Status != StatusSucceeded {
		t.Fatalf("saved output did not restore subscription metadata: %+v", job)
	}
	data, err := os.ReadFile(filepath.Join(st.Root, "job-meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"plan_type": "business_premium"`) && !strings.Contains(string(data), `"plan_type":"business_premium"`) {
		t.Fatal("recognized public tier was not persisted")
	}
	if strings.Contains(string(data), "private-access-fixture") || strings.Contains(string(data), "id_token") {
		t.Fatal("plan extraction leaked private token data to metadata")
	}
}

func TestUpdateResultChangesPlanButKeepsKnownTierOnMissingClaim(t *testing.T) {
	st, _ := store.New(t.TempDir())
	m := NewManager(context.Background(), 1, st, nil)
	job, err := m.Submit(Spec{ID: "saved", Email: "fixture@example.test", ImportOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	writePlanFixture(t, st.Root, job.ID, job.Email, "plus")
	m.UpdateResult(job.ID, true, "")
	current, _ := m.Get(job.ID)
	if current.PlanType != "plus" {
		t.Fatalf("fresh export tier=%q", current.PlanType)
	}
	writePlanFixture(t, st.Root, job.ID, job.Email, "pro")
	m.UpdateResult(job.ID, true, "")
	current, _ = m.Get(job.ID)
	if current.PlanType != "pro" {
		t.Fatalf("refreshed export tier=%q", current.PlanType)
	}
	writePlanFixture(t, st.Root, job.ID, job.Email, "")
	m.UpdateResult(job.ID, true, "")
	current, _ = m.Get(job.ID)
	if current.PlanType != "pro" {
		t.Fatal("missing refreshed claim erased prior known tier")
	}
	if plan := outputPlanType(st.Root, job.ID, "another@example.test"); plan != "unknown" {
		t.Fatal("another account's tier was exposed")
	}
}

func TestPlanMetadataSanitizesArbitraryValues(t *testing.T) {
	job := SanitizeJobForPublic(&Job{PlanType: "private-token-value"})
	if job.PlanType != "unknown" {
		t.Fatal("public plan field retained arbitrary data")
	}
}

func TestPlanInspectionRejectsSymlinksAndOversizedFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "oversized.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate((2 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := readPlanExport(path); err == nil {
		t.Fatal("oversized private file accepted")
	}
	link := filepath.Join(root, "linked.json")
	if err := os.Symlink(path, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := readPlanExport(link); err == nil {
		t.Fatal("symlink private file accepted")
	}
}

func TestRestorePreservesCachedPlanIfExistingExportHasNoPlan(t *testing.T) {
	st, _ := store.New(t.TempDir())
	writePlanFixture(t, st.Root, "saved", "fixture@example.test", "")
	if err := st.WriteJSON(context.Background(), "job-meta.json", map[string]any{"jobs": []any{map[string]any{"id": "saved", "email": "fixture@example.test", "plan_type": "pro", "status": StatusSucceeded}}}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(context.Background(), 1, st, nil)
	if err := m.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, _ := m.Get("saved")
	if job.PlanType != "pro" {
		t.Fatal("old output without claims erased cached known plan")
	}
}
