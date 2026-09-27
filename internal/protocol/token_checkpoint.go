package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"tosub2-go-port/internal/transport"
)

// A v1 extension keeps a redeemed grant in the private checkpoint until its
// normal legacy export is durable. Existing v1 stages remain readable. Tokens
// never enter public job events or the legacy codex session-export object.
const tokenCheckpointStage = "tokens_received"

// HasSavedOAuthGrant identifies a checkpoint that must be finalized without
// replaying authentication. Invalid contents still return true so callers
// cannot silently fall back to a new login or redeem an already-used code.
func HasSavedOAuthGrant(c Checkpoint) bool { return c.Stage == tokenCheckpointStage }

// FinalizeSavedOAuthGrant verifies and writes an already-redeemed grant using
// only local files. Call this before loading proxy credentials, choosing a TLS
// profile or starting Chromium. Existing browser/session bindings remain in
// the checkpoint but are not dependencies for exporting accepted tokens.
func FinalizeSavedOAuthGrant(ctx context.Context, c Checkpoint, options FlowOptions) (FlowResult, error) {
	if !HasSavedOAuthGrant(c) || options.WebOnly || options.Email == "" {
		return FlowResult{}, errors.New("CHECKPOINT_INVALID: no saved OAuth grant for this operation")
	}
	f := NewFlow(NewClient(savedGrantTransport{}, nil, ""), options)
	if err := f.Restore(c); err != nil {
		return FlowResult{}, err
	}
	return f.Run(ctx)
}

type savedGrantTransport struct{}

func (savedGrantTransport) Do(context.Context, transport.Request) (*transport.Response, error) {
	return nil, errors.New("CHECKPOINT_INVALID: saved OAuth grant recovery cannot make network requests")
}

func (f *Flow) saveTokenResponse(oauth map[string]any, tokens TokenSet) error {
	o := make(map[string]any, len(oauth)+3)
	for key, value := range oauth {
		o[key] = value
	}
	o["tokenResponse"] = tokens
	o["clientId"] = f.Options.ClientID
	o["redirectUri"] = f.Options.RedirectURI
	return f.save(tokenCheckpointStage, o)
}

func (f *Flow) restoreTokenResponse(c Checkpoint) (map[string]any, TokenSet, error) {
	invalid := errors.New("CHECKPOINT_INVALID: saved OAuth grant is incomplete or belongs to another client")
	o := c.OAuth
	if str(o["clientId"]) != f.Options.ClientID || str(o["redirectUri"]) != f.Options.RedirectURI || str(o["codeVerifier"]) == "" || !f.isCallback(str(o["callbackUrl"])) {
		return nil, TokenSet{}, invalid
	}
	if _, err := ParseOAuthCallback(str(o["callbackUrl"]), str(o["state"])); err != nil {
		return nil, TokenSet{}, invalid
	}
	b, err := json.Marshal(o["tokenResponse"])
	if err != nil {
		return nil, TokenSet{}, invalid
	}
	var tokens TokenSet
	if json.Unmarshal(b, &tokens) != nil || tokens.Validate() != nil {
		return nil, TokenSet{}, invalid
	}
	// These fields are mandatory in this Go-only stage, unlike optional
	// origins in legacy v1 login checkpoints. Never export a different
	// account's accepted grant under the resumed job's identity.
	if !strings.EqualFold(strings.TrimSpace(c.Email), strings.TrimSpace(f.Options.Email)) || c.AuthBase == "" || c.ChatGPTBase == "" ||
		strings.TrimRight(c.AuthBase, "/") != f.Options.AuthBase || strings.TrimRight(c.ChatGPTBase, "/") != f.Options.ChatGPTBase {
		return nil, TokenSet{}, invalid
	}
	claims, err := DecodeJWTClaims(tokens.IDToken)
	if err != nil {
		return nil, TokenSet{}, invalid
	}
	if email, _ := claims["email"].(string); email != "" && !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(c.Email)) {
		return nil, TokenSet{}, invalid
	}
	publicOAuth := make(map[string]any, len(o)-3)
	for key, value := range o {
		if key != "tokenResponse" && key != "clientId" && key != "redirectUri" {
			publicOAuth[key] = value
		}
	}
	return publicOAuth, tokens, nil
}
