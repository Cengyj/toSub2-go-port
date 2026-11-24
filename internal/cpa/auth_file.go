package cpa

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"tosub2-go-port/internal/authmaterial"
	"tosub2-go-port/internal/protocol"
)

const maxAuthFileBytes = 1 << 20

var (
	ErrUnsupportedAuthorization = errors.New("CPA 仅支持 ChatGPT/Codex 授权，BPS 或其他 OAuth 客户端的授权请先重新授权")
	ErrInvalidAuthorization     = errors.New("CPA 推送需要完整且身份一致的 OpenAI Codex OAuth 授权")
)

type AuthFile struct {
	Filename string
	Data     []byte
}

// These are the ordinary Codex credential fields accepted by CPA. Do not copy
// vendor settings, login cookies, passwords or 2FA material into this format.
type codexAuth struct {
	Type         string `json:"type"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	Expired      string `json:"expired,omitempty"`
	LastRefresh  string `json:"last_refresh,omitempty"`
	PlanType     string `json:"plan_type,omitempty"`
	Priority     int    `json:"priority"`
	Weight       int    `json:"weight"`
}

// BuildAuthFile converts one saved Sub2API-format account without mutating it.
// CPA refreshes ordinary Codex credentials with a fixed OAuth client ID, so a
// BPS token cannot be relabelled as Codex. Missing client identity is rejected
// unless the grant's JWT metadata identifies the supported client.
func BuildAuthFile(account map[string]any) (AuthFile, error) {
	if textValue(account["platform"]) != "openai" || textValue(account["type"]) != "oauth" {
		return AuthFile{}, ErrInvalidAuthorization
	}
	raw, err := json.Marshal(account)
	if err != nil || len(raw) > maxAuthFileBytes || authmaterial.ContainsLoginSessionJSON(raw) {
		return AuthFile{}, ErrInvalidAuthorization
	}
	creds, ok := account["credentials"].(map[string]any)
	if !ok {
		return AuthFile{}, ErrInvalidAuthorization
	}
	extra, _ := account["extra"].(map[string]any)
	access, refresh, id := textValue(creds["access_token"]), textValue(creds["refresh_token"]), textValue(creds["id_token"])
	if !validToken(access) || !validToken(refresh) || id != "" && !validToken(id) {
		return AuthFile{}, ErrInvalidAuthorization
	}
	accessClaims, _ := protocol.DecodeJWTClaims(access)
	idClaims, _ := protocol.DecodeJWTClaims(id)
	if err := validateClientIdentity(creds, extra, accessClaims, idClaims); err != nil {
		return AuthFile{}, err
	}
	if err := protocol.ValidateOAuthExportIdentity(creds, extra, ""); err != nil {
		return AuthFile{}, ErrInvalidAuthorization
	}
	meta := protocol.ExtractOAuthTokenMetadata(protocol.TokenSet{AccessToken: access, IDToken: id})
	email, err := consistentValue(true, textValue(creds["email"]), textValue(extra["email"]), meta.Email)
	if err != nil || !validEmail(email) {
		return AuthFile{}, ErrInvalidAuthorization
	}
	accountID, err := consistentValue(false, textValue(creds["chatgpt_account_id"]), textValue(creds["account_id"]), textValue(extra["chatgpt_account_id"]), textValue(extra["account_id"]), meta.ChatGPTAccountID)
	if err != nil || !validIdentifier(accountID) {
		return AuthFile{}, ErrInvalidAuthorization
	}
	expiry := meta.ExpiresAt
	if expiry <= 0 && creds["expires_at"] != nil && creds["expires_at"] != "" {
		var valid bool
		expiry, valid = unixSeconds(creds["expires_at"])
		if !valid {
			return AuthFile{}, ErrInvalidAuthorization
		}
	}
	doc := codexAuth{Type: "codex", AccessToken: access, RefreshToken: refresh, IDToken: id, AccountID: accountID, Email: strings.ToLower(email), PlanType: meta.PlanType, Weight: 1}
	if doc.PlanType == "" {
		doc.PlanType = textValue(creds["plan_type"])
		if doc.PlanType == "" {
			doc.PlanType = textValue(extra["plan_type"])
		}
	}
	if expiry > 0 {
		if expiry > 253402300799 {
			return AuthFile{}, ErrInvalidAuthorization
		}
		doc.Expired = time.Unix(expiry, 0).UTC().Format(time.RFC3339)
	}
	// Uploading is not a token refresh: never invent a current refresh time.
	if saved := textValue(creds["last_refresh"]); saved != "" {
		when, err := time.Parse(time.RFC3339, saved)
		if err != nil {
			return AuthFile{}, ErrInvalidAuthorization
		}
		doc.LastRefresh = when.UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(doc)
	if err != nil || len(data) > maxAuthFileBytes {
		return AuthFile{}, ErrInvalidAuthorization
	}
	return AuthFile{Filename: authFilename(doc.Email, doc.AccountID), Data: data}, nil
}

func validateClientIdentity(creds, extra, access, id map[string]any) error {
	if creds["openai_excel_bps"] == true || extra["openai_excel_bps"] == true {
		return ErrUnsupportedAuthorization
	}
	identities := []string{textValue(creds["client_id"]), textValue(extra["client_id"]), textValue(access["client_id"]), textValue(access["azp"]), textValue(id["client_id"]), textValue(id["azp"])}
	switch aud := id["aud"].(type) {
	case string:
		identities = append(identities, aud)
	case []any:
		if len(aud) == 0 {
			return ErrUnsupportedAuthorization
		}
		if len(aud) > 1 && textValue(id["azp"]) != protocol.DefaultCodexClientID {
			return ErrUnsupportedAuthorization
		}
		matched := false
		for _, value := range aud {
			audience, ok := value.(string)
			if !ok || strings.TrimSpace(audience) == "" {
				return ErrUnsupportedAuthorization
			}
			matched = matched || strings.TrimSpace(audience) == protocol.DefaultCodexClientID
		}
		if !matched {
			return ErrUnsupportedAuthorization
		}
		identities = append(identities, protocol.DefaultCodexClientID)
	}
	found := false
	for _, identity := range identities {
		if identity == "" {
			continue
		}
		if identity != protocol.DefaultCodexClientID {
			return ErrUnsupportedAuthorization
		}
		found = true
	}
	if !found {
		return ErrUnsupportedAuthorization
	}
	return nil
}

func validateAuthFile(file AuthFile) error {
	if len(file.Data) == 0 || len(file.Data) > maxAuthFileBytes || authmaterial.ContainsLoginSessionJSON(file.Data) {
		return ErrInvalidAuthorization
	}
	var doc codexAuth
	decoder := json.NewDecoder(bytes.NewReader(file.Data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&doc) != nil || doc.Type != "codex" || !validToken(doc.AccessToken) || !validToken(doc.RefreshToken) || doc.IDToken != "" && !validToken(doc.IDToken) || !validEmail(doc.Email) || !validIdentifier(doc.AccountID) {
		return ErrInvalidAuthorization
	}
	if file.Filename != authFilename(doc.Email, doc.AccountID) {
		return ErrInvalidAuthorization
	}
	if err := (UploadOptions{Priority: doc.Priority, Weight: &doc.Weight}).Validate(); err != nil {
		return err
	}
	return nil
}

// withUploadOptions produces a private request body. It never edits the saved
// grant or the caller's prepared file, and accepts only validated Codex fields.
func withUploadOptions(file AuthFile, options UploadOptions) (AuthFile, error) {
	if err := options.Validate(); err != nil {
		return AuthFile{}, err
	}
	if err := validateAuthFile(file); err != nil {
		return AuthFile{}, err
	}
	var doc codexAuth
	if err := json.Unmarshal(file.Data, &doc); err != nil {
		return AuthFile{}, ErrInvalidAuthorization
	}
	doc.Priority, doc.Weight = options.Priority, options.EffectiveWeight()
	data, err := json.Marshal(doc)
	if err != nil || len(data) > maxAuthFileBytes {
		return AuthFile{}, ErrInvalidAuthorization
	}
	return AuthFile{Filename: file.Filename, Data: data}, nil
}

// A stable, private name makes an explicit repeat upload replace this tool's
// same account file even when its plan changes. Other CPA files are untouched.
func authFilename(email, accountID string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email)) + "\x00" + accountID))
	return fmt.Sprintf("tosub2-codex-%x.json", digest)
}

func textValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func validToken(value string) bool {
	return value != "" && !strings.ContainsAny(value, " \t\r\n\x00")
}

func validEmail(value string) bool {
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value && len(value) <= 320 && !strings.ContainsAny(value, "\r\n\x00")
}

func validIdentifier(value string) bool {
	return value != "" && len(value) <= 1024 && !strings.ContainsAny(value, "\r\n\x00")
}

func consistentValue(fold bool, values ...string) (string, error) {
	result := ""
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if result != "" && value != result && !(fold && strings.EqualFold(value, result)) {
			return "", ErrInvalidAuthorization
		}
		result = value
	}
	return result, nil
}

func unixSeconds(value any) (int64, bool) {
	var number int64
	var err error
	switch v := value.(type) {
	case json.Number:
		number, err = v.Int64()
	case string:
		number, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			var parsed time.Time
			parsed, err = time.Parse(time.RFC3339, v)
			number = parsed.Unix()
		}
	case int:
		number = int64(v)
	case int64:
		number = v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || v < 0 || v > 253402300799 {
			return 0, false
		}
		number = int64(v)
	default:
		return 0, false
	}
	return number, err == nil && number >= 0 && number <= 253402300799
}
