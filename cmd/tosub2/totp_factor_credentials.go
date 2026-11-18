package main

import (
	"context"
	"errors"
	"strings"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/protocol"
)

// The enrollment journal is written before the callback. Read that protected
// account/operation record instead of deriving a binding from mfa_info, which
// can show a different device's default factor after concurrent enrollment.
func setupEnrollmentFactorID(path, email, secret, uri string) (string, error) {
	record, err := protocol.ReadTOTPResetJournal(path)
	if err != nil {
		return "", err
	}
	return enrollmentFactorID(record, email, secret, uri, false)
}

// Derived from protected evidence. Never copy this value into a display log.
type resetCredentialBinding struct {
	operationID, factorID, secret, uri string
}

func resetEnrollmentBinding(record map[string]any, email, secret, uri string) (resetCredentialBinding, error) {
	if record["enrollment_validated"] != true {
		return resetCredentialBinding{}, errors.New("TOTP_RECOVERY_CONFLICT: 登记材料尚未验证，未保存因子绑定")
	}
	factorID, err := enrollmentFactorID(record, email, secret, uri, true)
	if err != nil {
		return resetCredentialBinding{}, err
	}
	operationID, _ := record["operation_id"].(string)
	return resetCredentialBinding{operationID: operationID, factorID: factorID, secret: secret, uri: uri}, nil
}

func enrollmentFactorID(record map[string]any, email, secret, uri string, required bool) (string, error) {
	invalid := errors.New("TOTP_RECOVERY_CONFLICT: 密钥与登记因子证据不一致，未覆盖凭据")
	storedEmail, _ := record["email"].(string)
	operationID, _ := record["operation_id"].(string)
	sessionID, _ := record["session_id"].(string)
	if strings.TrimSpace(email) == "" || !strings.EqualFold(strings.TrimSpace(storedEmail), strings.TrimSpace(email)) || strings.TrimSpace(operationID) == "" || strings.TrimSpace(sessionID) == "" || secret == "" || record["secret"] != secret || record["otpauth_uri"] != uri {
		return "", invalid
	}
	factorID, _ := record["new_totp_factor_id"].(string)
	factor, hasFactor := record["enrollment_factor"].(map[string]any)
	// Legacy setup receipts did not include a factor ID. They remain unbound
	// until the login server accepts this seed against an advertised factor.
	if !required && factorID == "" && !hasFactor {
		return "", nil
	}
	if strings.TrimSpace(factorID) == "" || factor["id"] != factorID || factor["factor_type"] != "totp" || factor["is_recovery"] != false {
		return "", invalid
	}
	return factorID, nil
}

func saveBoundTOTPCredentials(ctx context.Context, vault credentials.Store, email string, value credentials.Credentials) error {
	if err := credentials.SaveContext(ctx, vault, email, value); err != nil {
		return &protocol.ResetStorageFailure{Stage: "local_commit", Reason: "STORAGE_WRITE_FAILED", Message: "TOTP_RESET_PERSISTENCE_REQUIRED: 新密钥及因子绑定未能安全保存，操作证据已保留"}
	}
	readback, err := credentials.LoadContext(ctx, vault, email)
	if err != nil || !credentials.EqualReadback(value, readback) {
		return &protocol.ResetStorageFailure{Stage: "local_commit", Reason: "STORAGE_READBACK_FAILED", Message: "TOTP_RESET_PERSISTENCE_REQUIRED: 新密钥及因子绑定读回失败或不一致，操作证据已保留"}
	}
	return nil
}

// Shared by receipt-confirmed resets and local-only completion. The caller
// holds the account lock; Manager independently checks the journal and vault.
func commitActivatedTOTP(ctx context.Context, vault credentials.Store, manager *jobs.Manager, job *jobs.Job, b resetCredentialBinding) error {
	c, err := credentials.LoadContext(ctx, vault, job.Email)
	if err != nil {
		return &protocol.ResetStorageFailure{Stage: "local_commit", Reason: "STORAGE_BLOCKED", Message: "TOTP_RESET_PERSISTENCE_REQUIRED: 无法读取本次操作的安全凭据"}
	}
	if b.operationID == "" || b.factorID == "" || b.secret == "" || c.TOTPResetOperationID != b.operationID {
		return errors.New("TOTP_COMPLETION_CONFLICT: 当前凭据不属于本次激活操作，未覆盖凭据")
	}
	needsSave := c.TOTPResetPending || c.TOTPFactorID == ""
	if c.TOTPResetPending {
		if c.PendingTOTPSecret != b.secret || c.PendingTOTPURI != b.uri || (c.PendingTOTPFactorID != "" && c.PendingTOTPFactorID != b.factorID) {
			return errors.New("TOTP_COMPLETION_CONFLICT: 待保存凭据与本次登记材料不一致")
		}
		c.TOTPSecret, c.TOTPResetPending = b.secret, false
		c.PendingTOTPSecret, c.PendingTOTPURI, c.PendingTOTPFactorID = "", "", ""
	} else if c.TOTPSecret != b.secret || (c.TOTPFactorID != "" && c.TOTPFactorID != b.factorID) || c.PendingTOTPSecret != "" || c.PendingTOTPURI != "" || c.PendingTOTPFactorID != "" {
		return errors.New("TOTP_COMPLETION_CONFLICT: 已保存的有效凭据与本次激活证据不一致，未覆盖凭据")
	}
	// A crash can leave an already-active seed. Backfill a missing legacy ID
	// only on the exact seed and operation; an exact match needs no vault write.
	if needsSave {
		c.TOTPFactorID = b.factorID
		if err := saveBoundTOTPCredentials(ctx, vault, job.Email, c); err != nil {
			return err
		}
	}
	return manager.CommitTOTPReset(ctx, job.ID, c)
}
