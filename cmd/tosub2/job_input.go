package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/mail"
)

// operationInput is confined to one protocol flow. Mail polling and manual
// input share the same consumed-code history and publish waiting prompts here.
type operationInput struct {
	cfg              config.Config
	manager          *jobs.Manager
	jobID            string
	emit             func(jobs.JobEvent)
	passwordAccepted func(string)
	mailURL          string
	mailRequest      mail.RequestConfig
	mailState        *mailInputState
	mailCutoff       int64
	requirements     map[string]bool
	manualAuth       bool
}

func newOperationInput(cfg config.Config, manager *jobs.Manager, jobID string, emit func(jobs.JobEvent), mailURL string, request mail.RequestConfig, passwordAccepted func(string)) *operationInput {
	return &operationInput{
		cfg: cfg, manager: manager, jobID: jobID, emit: emit, passwordAccepted: passwordAccepted,
		mailURL: mailURL, mailRequest: request, mailState: newMailInputState(),
		mailCutoff: time.Now().Add(-5 * time.Second).UnixMilli(), requirements: map[string]bool{},
	}
}

func (s *operationInput) mailEvent(status, message string) {
	// Mail progress is orthogonal to the protocol/input state. In particular,
	// late polling events must not reopen accepted input.
	s.emit(jobs.JobEvent{JobID: s.jobID, Data: map[string]any{"mailStatus": status, "mailApiError": message}, At: time.Now().UTC()})
}

func (s *operationInput) read(ctx context.Context, stage, prompt string) (string, error) {
	if stage == "email_otp" && s.mailURL != "" {
		s.requirements["emailOtp"] = true
		if !s.mailState.paused {
			s.emit(jobs.JobEvent{JobID: s.jobID, Stage: stage, Status: jobs.StatusWaitingInput, Prompt: "正在自动轮询邮箱验证码", Data: map[string]any{"mailStatus": "polling"}})
			value, err := pollMailInput(ctx, s.cfg, s.mailURL, s.mailRequest, s.mailCutoff, s.mailState, func() (string, bool) {
				value, ok := s.manager.ConsumeInput(s.jobID, "email_otp", "emailOtp", "code", "value")
				if ok {
					s.manualAuth = true
				}
				return value, ok
			}, s.mailEvent)
			if err == nil {
				return value, nil
			}
			if !errors.Is(err, errMailPollingTimeout) && !errors.Is(err, errMailPollingUnavailable) {
				return "", err
			}
			if errors.Is(err, errMailPollingUnavailable) {
				prompt = "收码接口不可用，已暂停自动轮询，请手动输入邮箱验证码或修正接口配置"
			} else {
				prompt = "自动收码等待超时，请手动输入邮箱验证码，或输入 r 重发"
			}
		}
	}
	s.manualAuth = true
	switch stage {
	case "email_otp":
		s.requirements["emailOtp"] = true
	case "totp":
		s.requirements["mfa"] = true
	case "password":
		s.requirements["password"] = true
	}
	s.emit(jobs.JobEvent{JobID: s.jobID, Stage: stage, Status: jobs.StatusWaitingInput, Prompt: prompt, At: time.Now().UTC()})
	keys := []string{stage, strings.ReplaceAll(stage, "-", "_"), "code", "value"}
	if stage == "email_otp" {
		keys = append(keys, "emailOtp")
	}
	if stage == "totp" {
		keys = append(keys, "mfa_otp")
	}
	if stage == "sms_otp" {
		keys = append(keys, "phone_otp")
	}
	if stage == "totp_setup" {
		keys = append(keys, "totp_setup_otp")
	}
	for {
		if value, ok := s.manager.ConsumeInput(s.jobID, keys...); ok {
			if stage == "password" {
				s.passwordAccepted(value)
			}
			return value, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (s *operationInput) beforeEmailOTP(ctx context.Context) error {
	if s.mailURL == "" {
		return nil
	}
	return s.mailState.baseline(ctx, s.mailURL, s.mailRequest, s.mailEvent)
}

func (s *operationInput) emailOTPRequested(requestedAt time.Time) {
	s.mailCutoff = requestedAt.Add(-5 * time.Second).UnixMilli()
	s.mailState.requested()
}

func (s *operationInput) authStep(name string, automatic bool) {
	s.requirements[name] = true
	if !automatic {
		s.manualAuth = true
	}
}
