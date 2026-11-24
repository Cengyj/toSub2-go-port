package console

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"tosub2-go-port/internal/jobs"
)

// parseInputExpectation keeps old clients compatible while current clients
// bind every response to the operation and prompt they actually displayed.
func parseInputExpectation(body map[string]any) (*jobs.InputExpectation, error) {
	raw, present := body["expectation"]
	if !present {
		return nil, nil
	}
	fields, ok := raw.(map[string]any)
	if !ok || len(fields) != 2 {
		return nil, errors.New("输入需要完整的操作时间和阶段")
	}
	operation, ok := fields["operationAt"].(string)
	stage, stageOK := fields["stage"].(string)
	at, err := time.Parse(time.RFC3339Nano, operation)
	if !ok || !stageOK || stage == "" || err != nil || at.IsZero() {
		return nil, errors.New("输入的操作时间或阶段无效")
	}
	return &jobs.InputExpectation{OperationAt: at, Stage: stage}, nil
}

func (s *Server) submitAccountInput(w http.ResponseWriter, r *http.Request, id string) {
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := rejectRemovedAuthenticationInputs(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	expected, err := parseInputExpectation(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	delete(body, "expectation")

	inputStage, inputValue, inputAction := "", "", ""
	if actionName, _ := body["action"].(string); actionName != "" {
		job, exists := s.jobs.Get(id)
		if !exists {
			writeError(w, 404, "job not found")
			return
		}
		value, _ := body["value"].(string)
		if actionName != "password" {
			value = strings.TrimSpace(value)
		}
		stage := actionName
		switch actionName {
		case "resend", "resend_email":
			stage = "email_otp"
			value = "r"
		case "resend_phone":
			stage = "phone_otp"
			value = "r"
		case "change_phone":
			stage = "phone_otp"
			value = "p"
		}
		allowed := map[string]bool{"email_otp": true, "phone": true, "phone_otp": true, "password": true, "mfa_otp": true, "totp_setup_otp": true}
		if !allowed[stage] {
			writeError(w, 400, "Unsupported input action")
			return
		}
		if job.Status != stage {
			writeError(w, 409, "任务当前不在对应输入阶段")
			return
		}
		if stage == "password" && value == "" {
			writeError(w, 400, "密码不能为空")
			return
		}
		if (actionName == "email_otp" || actionName == "mfa_otp" || actionName == "totp_setup_otp") && !regexp.MustCompile(`^[0-9]{6}$`).MatchString(value) {
			writeError(w, 400, "验证码必须为6位数字")
			return
		}
		if actionName == "phone_otp" && !regexp.MustCompile(`^[0-9]{4,8}$`).MatchString(value) {
			writeError(w, 400, "手机验证码必须为4到8位数字")
			return
		}
		if stage == "phone" && !regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`).MatchString(value) {
			writeError(w, 400, "手机号须使用E.164格式")
			return
		}
		inputStage, inputValue, inputAction = stage, value, actionName
		body = map[string]any{stage: value}
	}
	if inputStage != "" {
		var prepare func()
		if inputStage == "phone" || inputStage == "phone_otp" {
			prepare = func() {
				if inputStage == "phone" {
					s.prepareSMSPhoneInput(id, inputValue)
				} else if inputAction == "change_phone" {
					s.releaseSMS(id)
				}
				if inputStage == "phone_otp" {
					s.pauseSMSSubmission(id, inputValue)
				}
			}
		}
		accepted := false
		if expected != nil {
			if expected.Stage != inputStage {
				writeError(w, http.StatusConflict, "输入阶段已变化，请刷新后重试")
				return
			}
			accepted = s.jobs.InputForStageWithPreparationExpected(id, *expected, body, prepare)
		} else {
			accepted = s.jobs.InputForStageWithPreparation(id, inputStage, body, prepare)
		}
		if !accepted {
			writeError(w, 409, "任务当前不在对应输入阶段")
			return
		}
	} else if expected != nil {
		if !s.jobs.InputForStageExpected(id, *expected, body) {
			writeError(w, http.StatusConflict, "操作或输入阶段已变化，请刷新后重试")
			return
		}
	} else if !s.jobs.Input(id, body) {
		writeError(w, 404, "job not found")
		return
	}
	j, _ := s.jobs.Get(id)
	writeJSON(w, 200, map[string]any{"ok": true, "job": s.publicJob(j)})
}
