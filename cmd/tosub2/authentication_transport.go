package main

import (
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/protocol"
)

func validateAuthenticationCheckpoint(c protocol.Checkpoint, cfg config.Config, email string) error {
	// Flow.Restore performs the authoritative schema/account/origin checks in
	// an isolated jar. Run them before using any checkpoint cookie on network.
	probe := protocol.NewFlow(nil, protocol.FlowOptions{ChatGPTBase: cfg.ChatGPTBase, AuthBase: cfg.AuthBase, Email: email})
	return probe.Restore(c)
}

// A received code is bound by PKCE/state, not by browser cookies. It can be
// exchanged through Go HTTP without migrating the checkpoint's web session.
func cookieIndependentCallback(c *protocol.Checkpoint, operation string) bool {
	if c == nil || c.Stage != "callback_ready" {
		return false
	}
	switch operation {
	case "", "login", "full", "resume":
		return true
	}
	return false
}
