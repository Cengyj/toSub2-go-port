package protocol

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"tosub2-go-port/internal/transport"
)

// SetupTOTP enrolls and activates TOTP for the currently authenticated ChatGPT
// web session. The returned result is compatible with chatgpt-totp-setup.json.
func (f *Flow) SetupTOTP(ctx context.Context, resultPath string) (map[string]any, error) {
	if err := ValidateTOTPSetupStart(resultPath, f.Options.Email); err != nil {
		return nil, err
	}
	if err := f.startLoginOperation(ctx); err != nil {
		return nil, err
	}
	if _, err := f.LoginWeb(ctx); err != nil {
		return nil, err
	}
	return f.setupTOTPAttempt(ctx, resultPath)
}

// ValidateTOTPSetupStart protects the only enrollment/activation evidence from
// being overwritten by another login. An interrupted setup is not a new intent;
// confirmed material is completed locally by the application recovery path.
func ValidateTOTPSetupStart(path, email string) error {
	if path == "" {
		return nil // Explicit in-memory protocol callers do not have a journal.
	}
	prior, err := ReadTOTPResetJournal(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil && receiptStatus(prior["version"]) == 1 && prior["already_enabled"] == true && prior["reset_totp_requested"] != true && !TOTPResetJournalIsReadOnly(prior) &&
		strings.TrimSpace(email) != "" &&
		strings.EqualFold(strings.TrimSpace(str(prior["email"])), strings.TrimSpace(email)) &&
		prior["activation_succeeded"] != true {
		safe := true
		for _, key := range []string{"secret", "session_id", "new_totp_factor_id", "enrollment_factor", "activation_dispatch", "enrollment_dispatch"} {
			if _, exists := prior[key]; exists {
				safe = false
			}
		}
		if safe {
			return nil // A read-only, already-enabled result carries no candidate.
		}
	}
	return errors.New("TOTP_SETUP_PREVIOUS_RESULT: 已存在 2FA 设置或恢复记录；保留原记录，未重新登录、登记或激活")
}

// SetupTOTPSeedVerified distinguishes remote activation from proof of the saved
// seed. A manually supplied OTP can activate the remote factor without proving
// that the candidate protected locally is the correct seed. Old automatic-only
// receipts remain readable; an explicit manual/false/unknown schema never does.
func SetupTOTPSeedVerified(record map[string]any) bool {
	if record["activation_succeeded"] != true || record["seed_verified"] == false {
		return false
	}
	mode := str(record["activation_mode"])
	if schema, present := record["setup_evidence_schema"]; present {
		return receiptStatus(schema) == 1 && mode == "automatic" && record["seed_verified"] == true
	}
	return mode == "" || mode == "automatic"
}

func (f *Flow) setupTOTPAttempt(ctx context.Context, resultPath string) (map[string]any, error) {
	page, err := f.follow(ctx, f.Options.ChatGPTBase+"/?action=enable&factor=totp", f.Options.ChatGPTBase+"/")
	if err != nil {
		return nil, err
	}
	if f.isLoginURL(page.FinalURL) {
		return nil, expiredSession("TOTP_LOGIN_REQUIRED")
	}
	token, account, err := f.currentTOTPAuthorization(ctx, extractAccessToken(page.Response.Text()))
	if err != nil {
		return nil, err
	}
	h := mfaHeaders(f.Options.ChatGPTBase, token, f.deviceID, "/backend-api/accounts/mfa_info")
	var info map[string]any
	response, err := f.Client.JSON(ctx, http.MethodGet, f.Options.ChatGPTBase+"/backend-api/accounts/mfa_info", nil, RequestOptions{Headers: h, Referer: f.Options.ChatGPTBase + "/", DisableChallengeReplay: true, MaxResponseBytes: 256 << 10}, &info)
	if err != nil {
		return nil, err
	}
	var valid bool
	if response != nil {
		info, valid = decodeTOTPResponse(response.Body)
	}
	if !valid {
		return nil, errors.New("TOTP_STATUS_UNKNOWN: initial 2FA response is not a complete unambiguous object")
	}
	state := classifyTOTPState(info)
	if totpResponseRejected(info) || state.State == totpStateUnknown {
		return nil, errors.New("TOTP_STATUS_UNKNOWN: initial 2FA state is not confirmed; no enrollment was sent")
	}
	if state.State == totpStateEnabled {
		out := map[string]any{"version": 1, "operation_id": randomUUID(), "already_enabled": true, "email": f.Options.Email}
		if resultPath != "" {
			if err := writeTOTPJournal(resultPath, out); err != nil {
				return nil, err
			}
		}
		f.recordProgress("email_verified", nil)
		return out, nil
	}
	// Once enrollment may have been sent, keep its material and never restart
	// the operation. A changed response is not permission to enroll again.
	out := map[string]any{"version": 1, "operation_id": randomUUID(), "already_enabled": false,
		"setup_evidence_schema": 1, "enrollment_dispatch": "may_have_been_sent", "activation_dispatch": "not_sent",
		"activation_mode": "automatic", "activation_succeeded": false, "seed_verified": false, "email": f.Options.Email, "account_id": account}
	if resultPath != "" {
		if err := writeTOTPJournal(resultPath, out); err != nil {
			return out, err
		}
	}
	enroll, _, complete, requestErr := f.totpSetupMutation(ctx, token, "/backend-api/accounts/mfa/enroll", map[string]any{"factor_type": "totp"})
	secret, sid, factorID, materialErr := preserveSetupEnrollment(out, enroll, f.Options.Email)
	if resultPath != "" {
		if err := writeTOTPJournal(resultPath, out); err != nil {
			return out, errors.Join(requestErr, err)
		}
	}
	if requestErr != nil || !complete {
		return out, fmt.Errorf("TOTP_ENROLL_UNCONFIRMED: enrollment response was incomplete or rejected; required secret, session and factor material cannot be recovered by repeating enrollment: %w", requestErr)
	}
	if totpResponseRejected(enroll) {
		return out, errors.New("TOTP_ENROLL_REJECTED: enrollment was refused; no activation was sent")
	}
	if !totpOutcomeFieldsValid(enroll) {
		return out, errors.New("TOTP_ENROLL_UNCONFIRMED: enrollment outcome fields are inconsistent; no activation was sent")
	}
	if materialErr != nil {
		return out, materialErr
	}
	if f.Options.TOTPEnrollment != nil {
		if err := f.Options.TOTPEnrollment(ctx, secret, str(out["otpauth_uri"])); err != nil {
			return out, err
		}
	}
	code, err := GenerateTOTP(secret, time.Now())
	if err != nil {
		return out, err
	}
	for {
		out["activation_dispatch"] = "may_have_been_sent"
		if resultPath != "" {
			if err := writeTOTPJournal(resultPath, out); err != nil {
				return out, err
			}
		}
		if out["activation_mode"] == "automatic" {
			// Synchronous protection/readback can cross a TOTP time window.
			code, err = GenerateTOTP(secret, time.Now())
			if err != nil {
				return out, err
			}
		}
		act, response, complete, requestErr := f.totpSetupMutation(ctx, token, "/backend-api/accounts/mfa/user/activate_enrollment", map[string]any{"code": code, "factor_type": "totp", "session_id": sid})
		outcome := totpMutationUnknown
		if complete && response != nil && response.StatusCode >= 200 && response.StatusCode < 300 && requestErr == nil {
			outcome = classifyTOTPMutation("activation", act, factorID, sid)
		}
		if outcome == totpMutationSucceeded {
			out["activation_confirmation_source"] = "business_receipt"
			break
		}
		// Only an explicit code rejection can request another code. Unknown
		// JSON, network errors and generic HTTP 400 responses never take this path.
		if complete && response != nil && (response.StatusCode == http.StatusBadRequest || response.StatusCode >= 200 && response.StatusCode < 300) && totpResponseBindingMatches(act, factorID, sid) && totpInvalidCodeResponse(act) {
			out["activation_mode"] = "manual"
			out["activation_dispatch"] = "rejected"
			if resultPath != "" {
				if err := writeTOTPJournal(resultPath, out); err != nil {
					return out, err
				}
			}
			for {
				code, err = f.ask(ctx, "totp_setup", "2FA setup OTP (6 digits, q=quit)")
				if err != nil {
					return out, err
				}
				if digits6.MatchString(code) {
					break
				}
			}
			continue
		}
		if outcome == totpMutationRejected || complete && response != nil && response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout {
			if requestErr != nil {
				return out, requestErr
			}
			return out, errors.New("TOTP_ACTIVATION_REJECTED: activation was refused")
		}
		if errors.Is(requestErr, context.Canceled) || ctx.Err() != nil || transport.IsRequestDefinitelyUnsent(requestErr) {
			return out, errors.Join(requestErr, ctx.Err())
		}
		if factorID == "" {
			return out, fmt.Errorf("TOTP_ACTIVATION_UNKNOWN: enrollment did not supply a valid factor ID for read-only verification; the saved seed and session must be retained: %w", errors.Join(requestErr, errors.New("matching factor unavailable")))
		}
		// The legacy enrollment shape has no factor ID. Only the documented
		// factor-bearing shape can bind a read to this exact new enrollment.
		if _, err := f.waitForTOTPResetState(ctx, token, account, totpStateEnabled, factorID, ""); err != nil {
			return out, err
		}
		out["account_id"], out["activation_confirmation_source"], out["activation_confirmed_by_read"] = account, "state_read", true
		break
	}
	out["activation_succeeded"], out["activated_at"] = true, nowRFC3339()
	out["seed_verified"] = out["activation_mode"] == "automatic"
	if resultPath != "" {
		if err := writeTOTPJournal(resultPath, out); err != nil {
			return out, err
		}
	}
	if !SetupTOTPSeedVerified(out) {
		f.emit("totp_setup_seed_unverified", "running", "官方已确认人工验证码激活成功；保存的候选密钥尚未验证，保留待验证材料，不将其标记为当前有效密钥")
	}
	// A complete, bound receipt is sufficient; saving the confirmed seed never
	// depends on a login checkpoint or display refresh.
	f.recordProgress("email_verified", nil)
	return out, nil
}
