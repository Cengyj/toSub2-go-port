package protocol

import (
	"errors"
	"net/url"
	"strings"
)

const (
	AuthorizationMethodChatGPT = "chatgpt"
	AuthorizationMethodBPS     = "bps"
	DefaultBPSClientID         = "app_fnr0pYvVwwFDocDumLG3H2Bp"
	DefaultBPSRedirectURI      = "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback"
)

func NormalizeAuthorizationMethod(method string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "", AuthorizationMethodChatGPT:
		return AuthorizationMethodChatGPT, nil
	case AuthorizationMethodBPS:
		return AuthorizationMethodBPS, nil
	default:
		return "", errors.New("authorization method must be chatgpt or bps")
	}
}

// ApplyAuthorizationMethod selects the OAuth application's registered identity.
// It never changes ChatGPT's web-session endpoints used for security actions.
func ApplyAuthorizationMethod(options *FlowOptions) error {
	method, err := NormalizeAuthorizationMethod(options.AuthorizationMethod)
	if err != nil {
		return err
	}
	options.AuthorizationMethod = method
	if method == AuthorizationMethodBPS {
		options.ClientID, options.RedirectURI = DefaultBPSClientID, DefaultBPSRedirectURI
	} else {
		if options.ClientID == "" {
			options.ClientID = DefaultCodexClientID
		}
		if options.RedirectURI == "" {
			options.RedirectURI = DefaultCodexRedirectURI
		}
	}
	return nil
}

func BPSAuthorizationURL(authBase string, p PKCE) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	q := url.Values{
		"audience": {"https://api.openai.com/v1"}, "client_id": {DefaultBPSClientID},
		"code_challenge": {p.CodeChallenge}, "code_challenge_method": {"S256"},
		"platform": {"PC"}, "redirect_uri": {DefaultBPSRedirectURI}, "response_type": {"code"},
		"scope": {"openid offline_access email profile organization.read"}, "state": {p.State},
	}
	return strings.TrimRight(authBase, "/") + "/api/accounts/authorize?" + q.Encode(), nil
}

func (f *Flow) validateAuthorizationCheckpoint(c Checkpoint) error {
	method, err := NormalizeAuthorizationMethod(c.AuthorizationMethod)
	if err != nil || method != f.Options.AuthorizationMethod {
		return errors.New("CHECKPOINT_INVALID: authorization method mismatch; start a new login to change methods")
	}
	// Saved grants are validated again by restoreTokenResponse, which retains
	// the existing offline-finalization contract for old ChatGPT checkpoints.
	if method == AuthorizationMethodChatGPT && HasSavedOAuthGrant(c) {
		return nil
	}
	client, redirect := str(c.OAuth["clientId"]), str(c.OAuth["redirectUri"])
	if method == AuthorizationMethodBPS && (client == "" || redirect == "") || client != "" && client != f.Options.ClientID || redirect != "" && redirect != f.Options.RedirectURI {
		return errors.New("CHECKPOINT_INVALID: OAuth client or redirect mismatch")
	}
	return nil
}

func payloadCopy(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}
