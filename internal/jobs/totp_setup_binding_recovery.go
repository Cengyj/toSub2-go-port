package jobs

import (
	"errors"
	"strings"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/protocol"
)

var ErrTOTPSetupVerificationPending = errors.New("TOTP_SETUP_SEED_UNVERIFIED: 2FA 登记材料尚未核实，已保留候选密钥和恢复记录；请先处理原登记，不能再次设置或重置，也不能将旧密钥或候选密钥作为当前可用密钥导出")

// This runs under the account lease after the setup artifact has been read
// again and its activated result/account checked. A historical result cannot
// replace a newer credential without the matching pending enrollment.
func recoverSetupTOTPCredentials(current credentials.Credentials, record map[string]any) (credentials.Credentials, error) {
	if !protocol.SetupTOTPSeedVerified(record) {
		return current, ErrTOTPSetupVerificationPending
	}
	conflict := errors.New("2FA 设置结果与当前有效或待保存凭据不一致，未覆盖凭据，私有结果已保留")
	secret, uri := resultString(record, "secret"), resultString(record, "otpauth_uri")
	if secret == "" {
		return current, conflict
	}
	factorID, hasID := record["new_totp_factor_id"]
	factorValue, hasFactor := record["enrollment_factor"]
	boundJournal := hasID || hasFactor
	journalID := ""
	if boundJournal {
		var ok bool
		journalID, ok = factorID.(string)
		factor, validFactor := factorValue.(map[string]any)
		expectedURI, uriErr := protocol.BuildTOTPURI(resultString(record, "email"), secret)
		if !ok || strings.TrimSpace(journalID) == "" || !validFactor || factor["id"] != journalID || factor["factor_type"] != "totp" || factor["is_recovery"] != false ||
			strings.TrimSpace(resultString(record, "operation_id")) == "" || strings.TrimSpace(resultString(record, "session_id")) == "" || uriErr != nil || uri != expectedURI {
			return current, conflict
		}
	}
	pending := current.PendingTOTPSecret != "" || current.PendingTOTPURI != "" || current.PendingTOTPFactorID != ""
	if pending {
		// Legacy artifacts may omit the URI; their exact seed still identifies
		// the pending material, but never supplies a missing factor binding.
		if current.PendingTOTPSecret != secret || uri != "" && current.PendingTOTPURI != uri ||
			boundJournal && current.PendingTOTPFactorID != "" && current.PendingTOTPFactorID != journalID {
			return current, conflict
		}
	}
	if current.TOTPSecret != "" && current.TOTPSecret != secret && !pending {
		return current, conflict
	}
	activeID := ""
	if current.TOTPSecret == secret {
		activeID = current.TOTPFactorID
		if activeID != "" && (boundJournal && activeID != journalID || pending && current.PendingTOTPFactorID != "" && activeID != current.PendingTOTPFactorID) {
			return current, conflict
		}
	}
	if boundJournal && pending && current.PendingTOTPFactorID != "" {
		activeID = journalID
	}
	current.TOTPSecret, current.TOTPFactorID = secret, activeID
	current.PendingTOTPSecret, current.PendingTOTPURI, current.PendingTOTPFactorID = "", "", ""
	return current, nil
}
