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
func uploadTOTPWorkflow(ctx context.Context, root string, manager *jobs.Manager, job *jobs.Job, reporters ...func(jobs.JobEvent)) error {
	var retry func(transport.ReadRetryEvent)
	if len(reporters) > 0 && reporters[0] != nil {
		retry = networkRetryReporter(ctx, job.ID, "Sub2API 上传：", reporters[0])
	}
	return application.UploadSavedAuthorization(ctx, root, manager, job, retry)
}
