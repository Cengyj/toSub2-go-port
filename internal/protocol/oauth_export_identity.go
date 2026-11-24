package protocol

import (
	"errors"
	"strings"
)

var ErrOAuthExportIdentity = errors.New("OAuth 授权身份信息冲突或无效，请核实当前已保存授权")

// ValidateOAuthExportIdentity checks evidence already carried by a grant,
// independently of optional model inference. It performs no network request
// and does not validate JWT signatures. Opaque tokens and missing legacy
// metadata remain supported; explicit contradictions are never inferred away.
func ValidateOAuthExportIdentity(credentials, extra map[string]any, expectedEmail string) error {
	access, err := exportTokenClaims(credentials, "access_token")
	if err != nil {
		return err
	}
	id, err := exportTokenClaims(credentials, "id_token")
	if err != nil {
		return err
	}
	email := strings.TrimSpace(expectedEmail)
	for _, source := range append([]map[string]any{credentials, extra}, append(exportIdentityFields(access), exportIdentityFields(id)...)...) {
		if err := bindExportIdentity(&email, source, true, "email"); err != nil {
			return err
		}
	}
	client := ""
	for _, source := range []map[string]any{credentials, extra} {
		if err := bindExportIdentity(&client, source, false, "client_id"); err != nil {
			return err
		}
	}
	for _, source := range []map[string]any{access, id} {
		if err := bindExportIdentity(&client, source, false, "client_id", "azp"); err != nil {
			return err
		}
	}
	// Access-token aud identifies the resource, not the OAuth application.
	// ID-token aud is its recipient client and can be an audience list.
	if err := validateExportAudience(client, id); err != nil {
		return err
	}
	account := ""
	for _, source := range []map[string]any{credentials, extra} {
		if err := bindExportIdentity(&account, source, false, "chatgpt_account_id", "account_id"); err != nil {
			return err
		}
	}
	accessAccount := ""
	for _, source := range exportIdentityFields(access) {
		if err := bindExportIdentity(&accessAccount, source, false, "chatgpt_account_id", "account_id"); err != nil {
			return err
		}
	}
	if accessAccount != "" {
		// Refresh can retain an older ID token for a previous workspace. The
		// current access token is authoritative for that account binding.
		if account != "" && account != accessAccount {
			return ErrOAuthExportIdentity
		}
		return nil
	}
	for _, source := range exportIdentityFields(id) {
		if err := bindExportIdentity(&account, source, false, "chatgpt_account_id", "account_id"); err != nil {
			return err
		}
	}
	return nil
}

func validateExportAudience(client string, id map[string]any) error {
	audience, present := id["aud"]
	if !present || audience == nil {
		return nil
	}
	audiences, ok := audience.([]any)
	if !ok {
		audiences = []any{audience}
	}
	if len(audiences) == 0 {
		return ErrOAuthExportIdentity
	}
	if len(audiences) > 1 {
		// Multiple recipients are valid, but the authorized party must bind
		// the application. Other resource audiences are not client IDs.
		party, _ := id["azp"].(string)
		if strings.TrimSpace(party) == "" || strings.TrimSpace(party) != client {
			return ErrOAuthExportIdentity
		}
	}
	matched := client == "" && len(audiences) == 1
	for _, value := range audiences {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return ErrOAuthExportIdentity
		}
		matched = matched || strings.TrimSpace(text) == client
	}
	if !matched {
		return ErrOAuthExportIdentity
	}
	return nil
}

func exportIdentityFields(claims map[string]any) []map[string]any {
	fields := []map[string]any{claims}
	for _, key := range []string{"https://api.openai.com/auth", "auth", "https://api.openai.com/profile"} {
		if nested, ok := claims[key].(map[string]any); ok {
			fields = append(fields, nested)
		}
	}
	return fields
}

func bindExportIdentity(bound *string, source map[string]any, fold bool, keys ...string) error {
	for _, key := range keys {
		raw, present := source[key]
		if !present || raw == nil {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return ErrOAuthExportIdentity
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if *bound != "" && *bound != value && !(fold && strings.EqualFold(*bound, value)) {
			return ErrOAuthExportIdentity
		}
		*bound = value
	}
	return nil
}

func exportTokenClaims(credentials map[string]any, key string) (map[string]any, error) {
	raw, present := credentials[key]
	if !present || raw == nil || raw == "" {
		return nil, nil
	}
	token, ok := raw.(string)
	if !ok || strings.ContainsAny(token, " \t\r\n\x00") {
		return nil, ErrOAuthExportIdentity
	}
	if strings.Count(token, ".") != 2 {
		return nil, nil
	}
	claims, err := DecodeJWTClaims(token)
	if err != nil {
		return nil, ErrOAuthExportIdentity
	}
	return claims, nil
}
