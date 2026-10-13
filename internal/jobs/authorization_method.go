package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"tosub2-go-port/internal/protocol"
)

const (
	AuthorizationMethodChatGPT = "chatgpt"
	AuthorizationMethodBPS     = "bps"
)

var errUnknownAuthorizationClient = errors.New("无法识别账号的 OAuth client_id，请重新选择授权方式")

// NormalizeAuthorizationMethod keeps the authorization provider separate from
// LoginMode, which describes password versus email-code authentication.
func NormalizeAuthorizationMethod(method string) (string, error) {
	switch method {
	case "", AuthorizationMethodChatGPT:
		return AuthorizationMethodChatGPT, nil
	case AuthorizationMethodBPS:
		return AuthorizationMethodBPS, nil
	default:
		return "", fmt.Errorf("authorizationMethod 必须为 chatgpt 或 bps")
	}
}

func AuthorizationMethodForValues(current string, values map[string]any) (string, error) {
	if value, exists := values["authorizationMethod"]; exists {
		method, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("authorizationMethod 必须为 chatgpt 或 bps")
		}
		return NormalizeAuthorizationMethod(method)
	}
	return NormalizeAuthorizationMethod(current)
}

// AuthorizationMethodForClientIdentity reads the actual grant identity. An
// absent identity is distinct from the ChatGPT default; callers may retain an
// explicitly selected provider when an older export has no client metadata.
func AuthorizationMethodForClientIdentity(credentials, extra map[string]any) (string, error) {
	read := func(values map[string]any) (string, error) {
		value, exists := values["client_id"]
		if !exists || value == nil {
			return "", nil
		}
		client, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("OAuth client_id 格式无效")
		}
		return strings.TrimSpace(client), nil
	}
	client, err := read(credentials)
	if err != nil {
		return "", err
	}
	compatibilityClient, err := read(extra)
	if err != nil {
		return "", err
	}
	if client != "" && compatibilityClient != "" && client != compatibilityClient {
		return "", fmt.Errorf("账号的 OAuth client_id 信息冲突，请核实授权来源")
	}
	if client == "" {
		client = compatibilityClient
	}
	switch client {
	case "":
		return "", nil
	case protocol.DefaultBPSClientID:
		return AuthorizationMethodBPS, nil
	case protocol.DefaultCodexClientID:
		return AuthorizationMethodChatGPT, nil
	default:
		return "", errUnknownAuthorizationClient
	}
}

// The original ChatGPT CLI permits a custom Codex client. Discovery keeps
// these artifacts accessible without changing the client stored in the grant.
// Automatic remote repair uses the strict identity check above instead.
func restoredMethodForClientIdentity(credentials, extra map[string]any) (string, error) {
	method, err := AuthorizationMethodForClientIdentity(credentials, extra)
	if errors.Is(err, errUnknownAuthorizationClient) {
		return AuthorizationMethodChatGPT, nil
	}
	return method, err
}

func restoredAuthorizationMethod(root, id, email string, output, checkpoint bool) (string, error) {
	method := ""
	if output {
		data, err := readPlanExport(filepath.Join(root, id, "sub2api-import-oauth.json"))
		if err != nil {
			return "", err
		}
		var document struct {
			Accounts []struct {
				Name        string         `json:"name"`
				Credentials map[string]any `json:"credentials"`
				Extra       map[string]any `json:"extra"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			return "", err
		}
		if len(document.Accounts) > 0 {
			account := document.Accounts[0]
			credentialEmail, _ := account.Credentials["email"].(string)
			extraEmail, _ := account.Extra["email"].(string)
			credentialEmail, extraEmail = strings.TrimSpace(credentialEmail), strings.TrimSpace(extraEmail)
			if credentialEmail != "" && extraEmail != "" && !strings.EqualFold(credentialEmail, extraEmail) {
				return "", fmt.Errorf("授权导出中的账号邮箱信息冲突")
			}
			accountEmail := credentialEmail
			if accountEmail == "" {
				accountEmail = extraEmail
			}
			if accountEmail == "" {
				accountEmail = strings.TrimPrefix(strings.TrimSpace(account.Name), "oauth---")
			}
			if !strings.EqualFold(strings.TrimSpace(email), accountEmail) {
				return "", fmt.Errorf("授权导出的账号邮箱与本地任务不一致")
			}
			method, err = restoredMethodForClientIdentity(account.Credentials, account.Extra)
			if err != nil {
				return "", err
			}
		}
	}
	if checkpoint {
		data, err := readRegularJSON(filepath.Join(root, id, "login-checkpoint.json"))
		if err != nil {
			return "", err
		}
		var saved struct {
			Email               string         `json:"email"`
			AuthorizationMethod string         `json:"authorization_method"`
			OAuth               map[string]any `json:"oauth"`
		}
		if err := json.Unmarshal(data, &saved); err != nil {
			return "", err
		}
		if strings.EqualFold(strings.TrimSpace(saved.Email), strings.TrimSpace(email)) {
			clientMethod, err := restoredMethodForClientIdentity(map[string]any{"client_id": saved.OAuth["clientId"]}, nil)
			if err != nil {
				return "", err
			}
			checkpointMethod := saved.AuthorizationMethod
			if checkpointMethod != "" {
				checkpointMethod, err = NormalizeAuthorizationMethod(checkpointMethod)
				if err != nil {
					return "", err
				}
				if clientMethod != "" && clientMethod != checkpointMethod {
					return "", fmt.Errorf("恢复文件的授权方式与 OAuth client_id 不一致")
				}
			} else {
				checkpointMethod = clientMethod
			}
			if method != "" && checkpointMethod != "" && method != checkpointMethod {
				return "", fmt.Errorf("授权导出与恢复文件的授权方式不一致")
			}
			if checkpointMethod != "" {
				method = checkpointMethod
			}
		}
	}
	return NormalizeAuthorizationMethod(method)
}
