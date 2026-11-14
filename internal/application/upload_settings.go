package application

import (
	"errors"

	"tosub2-go-port/internal/cpa"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/sub2api"
)

type workflowUploadSettings struct {
	target  string
	sub2    sub2api.Config
	options sub2api.UploadOptions
	cpa     cpa.Config
}

func parseWorkflowUploadSettings(values map[string]any) (workflowUploadSettings, error) {
	var settings workflowUploadSettings
	target, err := jobs.UploadTargetForValues(values)
	if err != nil {
		return settings, err
	}
	settings.target = target
	if target == jobs.UploadTargetCPA {
		settings.cpa, err = cpa.ParseUploadSettings(values["cpaConfig"])
	} else {
		settings.sub2, settings.options, err = sub2api.ParseUploadSettings(values["sub2apiConfig"])
	}
	return settings, err
}

// ValidateUploadSettings validates the selected destination before any remote
// account operation. The configuration remains private to the current process.
func ValidateUploadSettings(values map[string]any) error {
	method, err := jobs.AuthorizationMethodForValues("", values)
	if err != nil {
		return err
	}
	return validateWorkflowUploadSettings(values, method)
}

func validateWorkflowUploadSettings(values map[string]any, method string) error {
	settings, err := parseWorkflowUploadSettings(values)
	if err != nil {
		return err
	}
	if settings.target == jobs.UploadTargetCPA && method == jobs.AuthorizationMethodBPS {
		return errors.New("CPA 仅支持 ChatGPT / Codex 授权，不能推送 BPS 授权")
	}
	return nil
}
