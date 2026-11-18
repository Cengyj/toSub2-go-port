package protocol

import (
	"errors"
	"strings"
)

// Authentication can advertise more than one independently valid TOTP factor.
// Neither list ordering nor mfa_info's displayed factor identifies the seed.
func selectTOTPLoginFactor(p authPayload, boundID string) (string, error) {
	session := payload(p["oai-client-auth-session"])
	ids := make([]string, 0)
	seen := make(map[string]bool)
	for _, key := range []string{"mfa_challenge_factors", "mfa_factors"} {
		for _, v := range slice(session[key]) {
			factor := payload(v)
			id := str(factor["id"])
			if str(factor["factor_type"]) != "totp" || factor["is_recovery"] == true || strings.TrimSpace(id) == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if boundID != "" {
		if seen[boundID] {
			return boundID, nil
		}
		return "", errors.New("MFA_FACTOR_BINDING_MISSING: 已保存密钥对应的 2FA 验证器不在本次登录返回的列表中；未尝试其他验证器，请核对账号当前的验证器")
	}
	if len(ids) == 0 {
		return "", errors.New("MFA_FACTOR_MISSING: TOTP factor ID missing")
	}
	if len(ids) > 1 {
		return "", errors.New("MFA_FACTOR_AMBIGUOUS: 账号返回多个 2FA 验证器，但当前密钥尚未保存对应的因子绑定；无法确定应验证哪一个，已停止，未轮流尝试验证码")
	}
	return ids[0], nil
}
