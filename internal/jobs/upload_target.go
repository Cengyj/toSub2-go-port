package jobs

import (
	"errors"
	"strings"
)

const (
	UploadTargetSub2API = "sub2api"
	UploadTargetCPA     = "cpa"
)

// NormalizeUploadTarget preserves Sub2API as the default for existing callers.
func NormalizeUploadTarget(target string) (string, error) {
	switch target {
	case "", UploadTargetSub2API:
		return UploadTargetSub2API, nil
	case UploadTargetCPA:
		return UploadTargetCPA, nil
	default:
		return "", errors.New("uploadTarget 必须为 sub2api 或 cpa")
	}
}

func UploadTargetForValues(values map[string]any) (string, error) {
	value, exists := values["uploadTarget"]
	if !exists {
		return UploadTargetSub2API, nil
	}
	target, ok := value.(string)
	if !ok {
		return "", errors.New("uploadTarget 必须为 sub2api 或 cpa")
	}
	return NormalizeUploadTarget(target)
}

func validateUploadTargetForOperation(values map[string]any, method string) error {
	target, err := UploadTargetForValues(values)
	if err != nil {
		return err
	}
	relogin, _ := values["uploadAfterRelogin"].(bool)
	totp, _ := values["uploadAfterTotp"].(bool)
	if (relogin || totp) && target == UploadTargetCPA && method == AuthorizationMethodBPS {
		return errors.New("CPA 仅支持 ChatGPT / Codex 授权，不能推送 BPS 授权")
	}
	return nil
}

func uploadTargetLabel(j *Job) string {
	if j.UploadTarget == UploadTargetCPA {
		return "CPA"
	}
	return "Sub2API"
}

func clearUploadConfiguration(j *Job) {
	delete(j.Input, "sub2apiConfig")
	delete(j.Input, "cpaConfig")
	delete(j.Input, "uploadTarget")
	delete(j.Input, "cpaRepairAccountId")
}

func initializeUploadTarget(j *Job, values map[string]any) {
	j.UploadTarget = ""
	if !j.UploadAfterRelogin && !j.UploadAfterTOTP {
		clearUploadConfiguration(j)
		return
	}
	j.UploadTarget, _ = UploadTargetForValues(values)
	if j.Input == nil {
		j.Input = map[string]any{}
	}
	j.Input["uploadTarget"] = j.UploadTarget
	if j.UploadTarget == UploadTargetCPA {
		delete(j.Input, "sub2apiConfig")
	} else {
		delete(j.Input, "cpaConfig")
		delete(j.Input, "cpaRepairAccountId")
	}
}

// Authentication answers may fill a prompt, but cannot rewrite an admitted
// operation's destination, credentials or follow-up steps. Match spelling
// variants as well so a future decoder cannot turn an ignored alias into an
// alternate way to change a running workflow.
func hasWorkflowControlInput(values map[string]any, includeAuthorization bool) bool {
	for key := range values {
		key = strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
		switch key {
		case "uploadtarget", "cpaconfig", "cparepairaccountid", "sub2apiconfig", "uploadafterrelogin", "uploadaftertotp", "reloginaftertotp", "logoutalldevicesaftertotp", "resettotp", "totpintentid", "operation":
			return true
		case "authorizationmethod":
			if includeAuthorization {
				return true
			}
		}
	}
	return false
}
