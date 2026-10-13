package main

import (
	"errors"
	"os"
	"strings"

	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/protocol"
)

// Security changes need a ChatGPT web session. The chosen OAuth method applies
// only when their optional follow-up starts a separate re-login operation.
func operationAuthorizationMethod(method, operation string) (string, error) {
	method, err := protocol.NormalizeAuthorizationMethod(method)
	if err != nil {
		return "", err
	}
	if strings.Contains(operation, "totp") || strings.Contains(operation, "2fa") || operation == "add-password" {
		return protocol.AuthorizationMethodChatGPT, nil
	}
	return method, nil
}

func authorizationProbeConfig(cfg config.Config, method string) config.Config {
	if method == protocol.AuthorizationMethodBPS {
		cfg.ChatGPTBase = cfg.AuthBase
	}
	return cfg
}

// Failed re-login leaves the previous usable export on disk. A later retry
// must not refresh that old client's token and call the new method successful.
func refreshUsesDifferentMethod(path, method, fallbackClient string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false // Existing refresh handling owns file and validation errors.
	}
	source, err := decodeRefreshDocument(data)
	if err != nil {
		return false
	}
	client, _, _, err := refreshSourceIdentity(source, fallbackClient)
	if err != nil {
		return false
	}
	return (client == protocol.DefaultBPSClientID) != (method == protocol.AuthorizationMethodBPS)
}

func cliAuthorizationMethod(c config.CLIConfig) (string, error) {
	method, err := protocol.NormalizeAuthorizationMethod(c.AuthorizationMethod)
	if err != nil {
		return "", err
	}
	var savedMethod string
	if c.ResumeCheckpoint != "" {
		checkpoint, err := protocol.ReadCheckpoint(c.ResumeCheckpoint)
		if err != nil {
			return "", err
		}
		savedMethod, err = protocol.NormalizeAuthorizationMethod(checkpoint.AuthorizationMethod)
		if err != nil {
			return "", err
		}
	} else if c.RefreshSub2API != "" {
		data, err := os.ReadFile(c.RefreshSub2API)
		if err != nil {
			return "", errors.New("cannot read OAuth import for refresh")
		}
		source, err := decodeRefreshDocument(data)
		if err != nil {
			return "", err
		}
		client, _, _, err := refreshSourceIdentity(source, protocol.DefaultCodexClientID)
		if err != nil {
			return "", err
		}
		savedMethod = protocol.AuthorizationMethodChatGPT
		if client == protocol.DefaultBPSClientID {
			savedMethod = protocol.AuthorizationMethodBPS
		}
	}
	if savedMethod != "" {
		if strings.TrimSpace(c.AuthorizationMethod) != "" && method != savedMethod {
			return "", errors.New("authorization method differs from the saved grant; start a new login to change methods")
		}
		method = savedMethod
	}
	if method == protocol.AuthorizationMethodBPS && (c.WebOnly || c.SetupTOTP || c.AddPassword) {
		return "", errors.New("BPS is an OAuth authorization method; web-only and security operations require ChatGPT")
	}
	return method, nil
}
