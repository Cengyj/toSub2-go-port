package console

import (
	"strings"
	"testing"
)

func TestFrontendProfileStagesNeverReplaceFinalOrSecurityOutcome(t *testing.T) {
	vm := presentationVM(t)
	for _, stage := range []string{"profile_required", "profile_submitting", "profile_completed", "profile_rejected", "profile_unknown", "profile_not_sent", "totp_setup_seed_unverified"} {
		active := presentFixture(t, vm, map[string]any{"status": "running", "stage": stage, "operationStatus": "running"})
		if active.Status.Code != stage || active.Summary == "" {
			t.Fatalf("missing active stage %s: %+v", stage, active)
		}
		for _, outcome := range []string{"failed", "canceled", "interrupted", "succeeded"} {
			final := presentFixture(t, vm, map[string]any{"status": "completed", "stage": stage, "operationStatus": outcome})
			if final.Status.Code == stage || !final.Terminal {
				t.Fatalf("stale stage %s replaced %s: %+v", stage, outcome, final)
			}
		}
		recovery := presentFixture(t, vm, map[string]any{"status": "running", "stage": stage, "operationStatus": "running", "totpCompletionPending": true})
		if recovery.Status.Code != "totp_completion_pending" {
			t.Fatalf("stage replaced local-save barrier: %+v", recovery)
		}
	}
	existing := presentFixture(t, vm, map[string]any{"status": "completed", "operationStatus": "succeeded", "lastOperationType": "add_password", "passwordAddOutcome": "already_exists"})
	if existing.Status.Code != "password_already_exists" || !strings.Contains(existing.Summary, "已跳过") {
		t.Fatalf("existing password presented as added: %+v", existing)
	}
}

func TestFrontendProfileLogsPreserveUnknownAndUnverifiedSeed(t *testing.T) {
	vm := jobLogsVM(t)
	for _, stage := range []string{"profile_unknown", "profile_rejected", "profile_not_sent", "totp_setup_seed_unverified"} {
		rows := parseLogsFixture(t, vm, []map[string]any{{"stage": stage, "status": "running", "message": "request timed out"}}, nil)
		if len(rows) != 1 || rows[0].Kind != "warning" || strings.Contains(rows[0].Message, "后重试") || rows[0].PhaseLabel == "" {
			t.Fatalf("unsafe stage translation %s: %+v", stage, rows)
		}
	}
}

func TestFrontendPelicanOriginalSVGDownloadPreservesSource(t *testing.T) {
	vm := frontendBrowserVM(t, "pelicanPreview")
	frontendPollingRun(t, vm, `
const original='<!-- synthetic --><svg xmlns="http://www.w3.org/2000/svg"><script>synthetic()</script></svg>';
const svg=Browser.pelicanDownload(original,"svg"),html=Browser.pelicanDownload(original,"html");
assert(svg.content===original&&svg.filename.endsWith(".svg")&&svg.type.startsWith("image/svg+xml"),"original SVG was wrapped or modified");
assert(html.content.includes(original)&&html.filename.endsWith(".html"),"HTML compatibility export lost original source");
let rejected=false;try{Browser.pelicanDownload('<html><body></body></html>',"svg");}catch{rejected=true;}assert(rejected,"HTML was mislabeled as SVG");
`)
}
