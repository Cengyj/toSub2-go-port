package main

import (
	"context"
	"errors"

	"tosub2-go-port/internal/console"
	"tosub2-go-port/internal/jobs"
)

// Optional sync runs inside the same scheduler lease as authorization. Its
// failure is recorded separately and never discards the newly saved grant.
func makeReloginUploadRunner(manager *jobs.Manager, operation, upload jobs.Runner) jobs.Runner {
	return func(ctx context.Context, job *jobs.Job, emit func(jobs.JobEvent)) (err error) {
		if job.Operation != "relogin" || !job.UploadAfterRelogin {
			return operation(ctx, job, emit)
		}
		if job.ReloginUploadStatus != "pending" {
			return errors.New("重新授权及自动上传流程不能自动重放，请重新选择操作")
		}
		finished := false
		defer func() {
			if !finished {
				if err != nil {
					values, _ := manager.InputCopy(job.ID)
					safe := jobs.SanitizeJobForPublic(&jobs.Job{Input: values, Error: err.Error()}).Error
					err = &totpWorkflowError{message: safe, cause: err}
				}
				err = errors.Join(err, manager.FinishReloginUpload(job.ID, err, false))
			}
		}()
		values, _ := manager.InputCopy(job.ID)
		if _, _, err = console.ParseTOTPSub2APIConfig(values["sub2apiConfig"]); err != nil {
			return err
		}
		if err = operation(ctx, job, emit); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = manager.StartReloginUpload(ctx, job.ID); err != nil {
			return err
		}
		uploadErr := upload(ctx, job, emit)
		uncertain := uploadErr != nil
		var outcome interface{ UploadOutcomeUncertain() bool }
		if errors.As(uploadErr, &outcome) {
			uncertain = outcome.UploadOutcomeUncertain()
		}
		finished = true
		// New authorization succeeded. Publish optional sync separately, even
		// if its response is rejected or its remote outcome is uncertain.
		return manager.FinishReloginUpload(job.ID, uploadErr, uncertain)
	}
}

type workflowUploadError struct {
	message   string
	cause     error
	uncertain bool
}

func (e *workflowUploadError) Error() string                { return e.message }
func (e *workflowUploadError) Unwrap() error                { return e.cause }
func (e *workflowUploadError) UploadOutcomeUncertain() bool { return e.uncertain }
