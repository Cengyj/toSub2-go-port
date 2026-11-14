package main

import (
	"tosub2-go-port/internal/accountlock"
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/jobs"
)

// Local archival and remote operations use the same account lock origin.
func configureAccountOperationLease(cfg config.Config, manager *jobs.Manager) {
	origin := cfg.ChatGPTBase
	if origin == "" {
		origin = "https://chatgpt.com"
	}
	manager.SetAccountOperationLease(func(email string) (func() error, error) {
		return accountlock.Acquire(origin, email)
	})
}
