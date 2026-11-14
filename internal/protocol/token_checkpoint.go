package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"tosub2-go-port/internal/authmaterial"
	"tosub2-go-port/internal/privatefile"
)

// SavedOAuthGrant is an already-issued result, not a login continuation. It
// deliberately has no Cookie, authorization code, PKCE, callback or web state.
type SavedOAuthGrant struct {
	Version             int      `json:"version"`
	Email               string   `json:"email"`
	AuthorizationMethod string   `json:"authorization_method"`
	AuthBase            string   `json:"auth_base"`
	ChatGPTBase         string   `json:"chatgpt_base"`
	ClientID            string   `json:"client_id"`
	RedirectURI         string   `json:"redirect_uri"`
	Tokens              TokenSet `json:"tokens"`
}

func HasSavedOAuthGrant(value any) bool {
	_, ok := value.(SavedOAuthGrant)
	return ok
}

func (g SavedOAuthGrant) validate(options FlowOptions) error {
	invalid := errors.New("OAUTH_GRANT_INVALID: issued token result is incomplete or belongs to another account or client")
	if g.Version != 1 || strings.TrimSpace(g.Email) == "" || g.Tokens.AccessToken == "" || g.AuthBase == "" || g.ChatGPTBase == "" || g.ClientID == "" || g.RedirectURI == "" || g.AuthorizationMethod == "" {
		return invalid
	}
	if err := ApplyAuthorizationMethod(&options); err != nil {
		return err
	}
	method, err := NormalizeAuthorizationMethod(g.AuthorizationMethod)
	if err != nil || method != options.AuthorizationMethod || g.ClientID != options.ClientID || g.RedirectURI != options.RedirectURI ||
		!strings.EqualFold(strings.TrimSpace(g.Email), strings.TrimSpace(options.Email)) ||
		strings.TrimRight(g.AuthBase, "/") != strings.TrimRight(options.AuthBase, "/") || strings.TrimRight(g.ChatGPTBase, "/") != strings.TrimRight(options.ChatGPTBase, "/") {
		return invalid
	}
	if g.ClientID != DefaultBPSClientID && g.Tokens.Validate() != nil {
		return invalid
	}
	claims, err := DecodeJWTClaims(g.Tokens.IDToken)
	if err != nil && g.ClientID == DefaultBPSClientID {
		claims, err = DecodeJWTClaims(g.Tokens.AccessToken)
	}
	if err != nil {
		return invalid
	}
	if email := claimsEmail(claims); email != "" && !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(g.Email)) {
		return invalid
	}
	// Access-token metadata takes precedence during export. An explicit email
	// there must not silently change the account selected by this issued result.
	if access, err := DecodeJWTClaims(g.Tokens.AccessToken); err == nil {
		if email := claimsEmail(access); email != "" && !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(g.Email)) {
			return invalid
		}
	}
	return nil
}

func ReadSavedOAuthGrant(path string) (SavedOAuthGrant, error) {
	var grant SavedOAuthGrant
	file, err := os.Open(path)
	if err != nil {
		return grant, err
	}
	defer file.Close()
	const maxSavedGrantBytes = 2 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxSavedGrantBytes+1))
	if err != nil {
		return grant, err
	}
	if len(data) > maxSavedGrantBytes || authmaterial.ValidateUnambiguousJSON(data) != nil || !savedGrantFieldsValid(data) {
		return grant, errors.New("OAUTH_GRANT_INVALID: issued token result has an unsupported or ambiguous format")
	}
	// Reject unknown fields rather than preserving old session material.
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&grant); err != nil {
		return SavedOAuthGrant{}, errors.New("OAUTH_GRANT_INVALID: issued token result has an unsupported format")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return SavedOAuthGrant{}, errors.New("OAUTH_GRANT_INVALID: multiple or trailing JSON values")
	}
	options := FlowOptions{Email: grant.Email, AuthorizationMethod: grant.AuthorizationMethod, ClientID: grant.ClientID, RedirectURI: grant.RedirectURI, AuthBase: grant.AuthBase, ChatGPTBase: grant.ChatGPTBase}
	if err = grant.validate(options); err != nil {
		return SavedOAuthGrant{}, err
	}
	return grant, nil
}

func WriteSavedOAuthGrant(path string, grant SavedOAuthGrant) error {
	options := FlowOptions{Email: grant.Email, AuthorizationMethod: grant.AuthorizationMethod, ClientID: grant.ClientID, RedirectURI: grant.RedirectURI, AuthBase: grant.AuthBase, ChatGPTBase: grant.ChatGPTBase}
	if err := grant.validate(options); err != nil {
		return err
	}
	return writeVerifiedOAuthJSON(path, grant)
}

// encoding/json accepts case-insensitive aliases for struct fields. A saved
// grant has one exact schema; aliases must not create duplicate logical fields.
func savedGrantFieldsValid(data []byte) bool {
	root, valid := recentAuthObject(data)
	if !valid {
		return false
	}
	for key := range root {
		switch key {
		case "version", "email", "authorization_method", "auth_base", "chatgpt_base", "client_id", "redirect_uri", "tokens":
		default:
			return false
		}
	}
	tokens, valid := recentAuthObject(root["tokens"])
	if !valid {
		return false
	}
	for key := range tokens {
		switch key {
		case "access_token", "refresh_token", "id_token", "token_type", "expires_in":
		default:
			return false
		}
	}
	return true
}

func validateOAuthResultPaths(options FlowOptions) error {
	if options.OutputPath == "" || options.GrantPath == "" {
		return nil
	}
	output, err := filepath.Abs(options.OutputPath)
	if err != nil {
		return err
	}
	grant, err := filepath.Abs(options.GrantPath)
	if err != nil {
		return err
	}
	conflict := output == grant
	if !conflict {
		outputInfo, outputErr := os.Stat(output)
		grantInfo, grantErr := os.Stat(grant)
		conflict = outputErr == nil && grantErr == nil && os.SameFile(outputInfo, grantInfo)
	}
	if conflict {
		return errors.New("OAUTH_GRANT_OUTPUT_CONFLICT: issued grant and final authorization require distinct paths")
	}
	return nil
}

func verifySavedOAuthGrant(path string, expected SavedOAuthGrant) error {
	if path == "" {
		return nil
	}
	stored, err := ReadSavedOAuthGrant(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // An in-memory result or an already-retired grant is valid.
	}
	if err != nil {
		return err
	}
	if stored != expected {
		return errors.New("OAUTH_GRANT_CHANGED: local issued result differs; no saved grant was removed")
	}
	return nil
}

// A grant is retired only after the final result was durably written and the
// exact bytes were read back. A failure retains the already-issued tokens.
func writeVerifiedOAuthJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	ctx, cancel := privateStateSaveContext()
	defer cancel()
	if err := privatefile.Write(ctx, path, data); err != nil {
		return err
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(stored, data) {
		return errors.New("OAUTH_RESULT_READBACK_FAILED: local authorization result did not match its write")
	}
	return nil
}

// FinalizeSavedOAuthGrant has no transport and can only save an issued result.
func FinalizeSavedOAuthGrant(ctx context.Context, value any, options FlowOptions) (FlowResult, error) {
	grant, ok := value.(SavedOAuthGrant)
	if !ok {
		return FlowResult{}, ErrLoginPersistenceRemoved
	}
	if options.ChatGPTBase == "" {
		options.ChatGPTBase = "https://chatgpt.com"
	}
	if options.AuthBase == "" {
		options.AuthBase = "https://auth.openai.com"
	}
	options.ChatGPTBase = strings.TrimRight(options.ChatGPTBase, "/")
	options.AuthBase = strings.TrimRight(options.AuthBase, "/")
	if err := ApplyAuthorizationMethod(&options); err != nil {
		return FlowResult{}, err
	}
	if options.OutputMode != "" && options.OutputMode != "sub2api" {
		return FlowResult{}, ErrLoginPersistenceRemoved
	}
	if err := grant.validate(options); err != nil {
		return FlowResult{}, err
	}
	if err := validateOAuthResultPaths(options); err != nil {
		return FlowResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return FlowResult{}, err
	}
	result, err := BuildSub2APIOAuthExport(grant.Tokens, grant.ClientID, grant.Email, options.AccountName, options.Concurrency, options.Priority, options.RateMultiplier)
	if err != nil {
		return FlowResult{}, err
	}
	if options.OutputPath != "" {
		if err := verifySavedOAuthGrant(options.GrantPath, grant); err != nil {
			return FlowResult{}, err
		}
		if err := writeVerifiedOAuthJSON(options.OutputPath, result); err != nil {
			return FlowResult{}, err
		}
		if options.GrantPath != "" {
			if err := verifySavedOAuthGrant(options.GrantPath, grant); err != nil {
				return FlowResult{}, err
			}
			if err := RemovePrivateResult(options.GrantPath); err != nil {
				return FlowResult{}, err
			}
		}
	}
	return FlowResult{Export: result}, nil
}

func (f *Flow) oauthGrant(tokens TokenSet) SavedOAuthGrant {
	return SavedOAuthGrant{Version: 1, Email: f.Options.Email, AuthorizationMethod: f.Options.AuthorizationMethod, AuthBase: f.Options.AuthBase, ChatGPTBase: f.Options.ChatGPTBase, ClientID: f.Options.ClientID, RedirectURI: f.Options.RedirectURI, Tokens: tokens}
}
func (f *Flow) saveOAuthGrant(grant SavedOAuthGrant) error {
	if f.Options.SaveGrant != nil {
		return f.Options.SaveGrant(grant)
	}
	if f.Options.GrantPath != "" {
		return WriteSavedOAuthGrant(f.Options.GrantPath, grant)
	}
	return nil
}
