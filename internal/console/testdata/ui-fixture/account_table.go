package main

// These fixed scenarios use only the fixture's synthetic accounts. No login,
// upload or upstream transport is involved in visual state verification.
func setTableOperation(job map[string]any, scenario string) {
	switch scenario {
	case "completed", "sync_failed", "sync_unknown", "queued", "running", "waiting":
	default:
		return
	}
	job["lastOperationType"] = "relogin"
	job["status"], job["operationStatus"] = "completed", "succeeded"
	job["canDownload"], job["resultSaved"] = true, true
	job["operationError"], job["prompt"], job["reloginUploadStatus"] = "", "", ""
	job["uploadAfterRelogin"] = false
	switch scenario {
	case "sync_failed", "sync_unknown":
		job["uploadAfterRelogin"] = true
		if scenario == "sync_failed" {
			job["reloginUploadStatus"] = "failed"
			job["reloginUploadError"] = "合成同步失败；已保存授权保持有效，请核对远端账号。"
		} else {
			job["reloginUploadStatus"] = "unknown"
			job["reloginUploadError"] = "合成同步响应丢失；需要核实远端结果，不自动重发。"
		}
	case "queued", "running":
		job["status"], job["operationStatus"] = scenario, scenario
		job["queuePosition"] = 2
	case "waiting":
		job["status"], job["operationStatus"] = "email_otp", "waiting"
		job["prompt"] = "请输入合成邮箱验证码继续本次授权"
	}
}
