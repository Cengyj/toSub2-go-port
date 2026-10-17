package console

import (
	"context"
	"errors"
	"net/http"

	"tosub2-go-port/internal/jobs"
)

type batchSubmissionFailure struct {
	ID        string `json:"id"`
	Error     string `json:"error"`
	Uncertain bool   `json:"uncertain,omitempty"`
}

// These counts describe submission, never completion of authorization or 2FA.
type batchSubmissionResult struct {
	OK        bool                     `json:"ok"`
	Outcome   string                   `json:"outcome"`
	Selected  int                      `json:"selected"`
	Queued    int                      `json:"queued"`
	Started   int                      `json:"started"` // Retained for existing console clients.
	Skipped   int                      `json:"skipped"`
	Failed    int                      `json:"failed"`
	Unknown   int                      `json:"unknown"`
	Uncertain bool                     `json:"uncertain"`
	Deleted   int                      `json:"deleted"` // Historical non-delete response alias.
	Jobs      []map[string]any         `json:"jobs"`
	Failures  []batchSubmissionFailure `json:"failures"`
	Error     string                   `json:"error,omitempty"`
}

func batchSubmissionMessage(err error) string {
	switch {
	case errors.Is(err, jobs.ErrAdmissionUncertain):
		return "本地保存结果待核实；当前进程未启动新操作，重启可能恢复磁盘排队，请先核实，避免重复提交"
	case errors.Is(err, jobs.ErrTOTPRecoveryPending), errors.Is(err, jobs.ErrTOTPRecoveryRequired):
		return "2FA 恢复尚未完成，请先核实并完成恢复"
	case errors.Is(err, context.Canceled):
		return "请求已取消，任务未提交"
	case errors.Is(err, context.DeadlineExceeded):
		return "提交等待超时，请刷新账号状态后重试"
	default:
		// Storage errors can contain private paths or values. Do not expose
		// arbitrary backend error text through the bulk response.
		return "任务提交失败，请刷新账号状态并检查凭据或存储后重试"
	}
}

func writeBatchSubmissionResult(w http.ResponseWriter, selected, skipped int, queued []map[string]any, failures []batchSubmissionFailure) {
	result := batchSubmissionResult{OK: len(failures) == 0 && len(queued) > 0, Outcome: "queued", Selected: selected, Queued: len(queued), Started: len(queued), Skipped: skipped, Failed: len(failures), Deleted: len(queued), Jobs: queued, Failures: failures}
	for _, failure := range failures {
		if failure.Uncertain {
			result.Unknown++
			result.Failed--
		}
	}
	result.Uncertain = result.Unknown > 0
	status := http.StatusOK
	if len(failures) > 0 {
		result.Outcome = "partial"
	}
	if len(queued) == 0 {
		status = http.StatusConflict
		result.Outcome, result.Error = "skipped", "选中的任务当前均无法执行该操作"
		if len(failures) > 0 {
			result.Outcome, result.Error = "failed", "未能提交所选任务，请查看失败项并刷新状态后重试"
		}
		if result.Uncertain {
			result.Outcome, result.Error = "unknown", "部分任务提交结果待核实，请先检查任务状态，避免重复提交"
		}
	}
	writeJSON(w, status, result)
}
