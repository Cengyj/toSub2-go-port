package main

import (
	"context"
	"tosub2-go-port/internal/application"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/transport"
)

func makeTOTPWorkflowRunner(manager *jobs.Manager, vault credentials.Store, operation, upload jobs.Runner) jobs.Runner {
	return application.TOTPWorkflowRunner(manager, vault, operation, upload)
}
func uploadTOTPWorkflowAtOrigin(ctx context.Context, root, origin string, manager *jobs.Manager, job *jobs.Job, reporters ...func(jobs.JobEvent)) error {
	var retry func(transport.ReadRetryEvent)
	if len(reporters) > 0 && reporters[0] != nil {
		label := "Sub2API"
		if job.UploadTarget == jobs.UploadTargetCPA {
			label = "CPA"
		}
		retry = networkRetryReporter(ctx, job.ID, label+" 上传：", reporters[0])
	}
	return application.UploadSavedAuthorizationAtOrigin(ctx, root, origin, manager, job, retry)
}
