package console

import (
	"fmt"

	"tosub2-go-port/internal/jobs"
)

func normalizeReloginUpload(action string, values map[string]any) (bool, error) {
	value, present := values["uploadAfterRelogin"]
	if !present {
		return false, nil
	}
	enabled, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("uploadAfterRelogin 必须是布尔值")
	}
	if enabled && action != "relogin" {
		return false, fmt.Errorf("重登后自动上传只能用于重新登录授权")
	}
	if enabled {
		if _, _, err := ParseTOTPSub2APIConfig(values["sub2apiConfig"]); err != nil {
			return false, err
		}
	}
	return enabled, nil
}

func publicReloginUpload(j *jobs.Job, out map[string]any) map[string]any {
	out["uploadAfterRelogin"] = j.UploadAfterRelogin
	out["reloginAuthorizationSaved"] = j.ReloginAuthorizationSaved
	out["reloginUploadStatus"] = j.ReloginUploadStatus
	out["reloginUploadError"] = j.ReloginUploadError
	return out
}
