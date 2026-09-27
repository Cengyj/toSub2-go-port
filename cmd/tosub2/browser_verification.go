package main

import (
	"errors"

	"tosub2-go-port/internal/challenge"
	"tosub2-go-port/internal/jobs"
)

// A page observation only authorizes one protected-request replay in the same
// browser. The protocol client must still check the actual HTTP response.
func validateBrowserReplayReadiness(result challenge.Result, err error) error {
	if err != nil {
		return err
	}
	if !result.SessionBound || (!result.Solved && !result.RetryReady) {
		return errors.New("BROWSER_VERIFICATION_UNCONFIRMED: no same-session evidence permits a protected request retry")
	}
	return nil
}

func browserVerificationFailureEvent(jobID string, err error) jobs.JobEvent {
	stage := "browser_verification_failed"
	prompt := "浏览器验证未能完成；本次等待或重试已结束。请检查错误原因后手动重试。"
	switch {
	case errors.Is(err, challenge.ErrVerificationRejected):
		stage = "browser_verification_rejected"
		prompt = "网站拒绝了请求，未检测到可交互验证；已停止重复重试。"
	case errors.Is(err, challenge.ErrVerificationPending):
		stage = "browser_verification_incomplete"
		prompt = "本次网站验证等待已结束，验证仍未完成；已停止自动重试，请确认验证窗口和网络后手动重试。"
	}
	// SecurityCheckRequired also controls checkpoint resume semantics. Do not
	// repurpose that phone-flow flag merely to change this status message.
	return jobs.JobEvent{JobID: jobID, Stage: stage, Status: jobs.StatusRunning, Prompt: prompt}
}
