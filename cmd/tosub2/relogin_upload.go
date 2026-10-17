package main

import (
	"tosub2-go-port/internal/application"
	"tosub2-go-port/internal/jobs"
)

func makeReloginUploadRunner(manager *jobs.Manager, operation, upload jobs.Runner) jobs.Runner {
	return application.ReloginUploadRunner(manager, operation, upload)
}
