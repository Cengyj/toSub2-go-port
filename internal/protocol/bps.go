package protocol

import (
	"context"
	"errors"
	"strings"
	"time"
)

// RunBPSOAuth logs directly into the BPS OAuth application. It does not visit
// ChatGPT or exchange the code through the Codex client.
func (f *Flow) RunBPSOAuth(ctx context.Context) (map[string]any, error) {
	if f.checkpoint != nil && !strings.HasPrefix(f.checkpoint.Stage, "bps_") {
		return f.ResumeOAuth(ctx, *f.checkpoint)
	}
	var o map[string]any
	var current authPayload
	var pageURL string
	if f.checkpoint != nil {
		o = f.checkpoint.OAuth
		current = payload(o["current"])
		pageURL = str(o["pageURL"])
		if str(o["codeVerifier"]) == "" || str(o["state"]) == "" {
			return nil, errors.New("CHECKPOINT_INVALID: missing BPS PKCE state")
		}
	} else {
		f.emit("bps_login", "running", "正在使用 BPS 登录授权")
		p, err := newPKCE(64, 32)
		if err != nil {
			return nil, err
		}
		p.State = "bps." + p.State + ".PC"
		raw, err := BPSAuthorizationURL(f.Options.AuthBase, p)
		if err != nil {
			return nil, err
		}
		o = map[string]any{"codeVerifier": p.CodeVerifier, "state": p.State}
		page, err := f.follow(ctx, raw, "")
		if err != nil {
			return nil, err
		}
		if f.isCallback(page.FinalURL) {
			return f.finishCallback(page.FinalURL, o)
		}
		pageURL = page.FinalURL
		f.deviceID = f.Client.Jar.Value("oai-did", f.Options.AuthBase+"/")
		if f.deviceID == "" {
			f.deviceID = randomUUID()
		}
		f.web = map[string]any{"deviceId": f.deviceID}
		if page.Response != nil && pathOf(pageURL) == "/choose-an-account" {
			id := ExtractSessionID(page.Response.Text())
			if id == "" {
				return nil, errors.New("SESSION_SELECTION_INVALID: BPS account session missing")
			}
			current, err = f.auth(ctx, "/api/accounts/session/select", map[string]any{"session_id": id}, pageURL, "")
			if err != nil {
				return nil, err
			}
		} else if pathOf(pageURL) != "/log-in" && pathOf(pageURL) != "/log-in-or-create-account" && pathOf(pageURL) != "/log-in/password" && pathOf(pageURL) != "/email-verification" {
			return nil, errors.New("BPS_LOGIN_PAGE_INVALID: unsupported login page")
		}
		o["pageURL"], o["current"] = pageURL, current
		if err = f.save("bps_login", o); err != nil {
			return nil, err
		}
	}
	for steps := 0; steps < 12; steps++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var next authPayload
		var err error
		ref := continuation(current)
		if ref == "" {
			ref = pageURL
		}
		if f.isCallback(ref) {
			return f.finishCallback(ref, o)
		}
		if current == nil && (pathOf(pageURL) == "/log-in" || pathOf(pageURL) == "/log-in-or-create-account") {
			if err = f.beforeEmailOTP(ctx); err != nil {
				return nil, err
			}
			requestedAt := time.Now().UTC()
			next, err = f.auth(ctx, "/api/accounts/authorize/continue", map[string]any{"username": map[string]any{"kind": "email", "value": f.Options.Email}}, pageURL, "authorize_continue")
			if err == nil && (pageType(next) == "email_verification" || pathOf(continuation(next)) == "/email-verification") {
				f.emailOTPRequested(requestedAt)
			}
		} else {
			switch {
			case pageType(current) == "login_password" || pathOf(ref) == "/log-in/password":
				next, err = f.verifyPassword(ctx, ref)
			case pageType(current) == "email_verification" || pathOf(ref) == "/email-verification":
				if stamp, parseErr := time.Parse(time.RFC3339Nano, str(f.web["emailOtpRequestedAt"])); parseErr == nil && f.Options.EmailOTPRequested != nil {
					f.Options.EmailOTPRequested(stamp)
				}
				next, err = f.verifyEmail(ctx, ref)
			case pageType(current) == "mfa_challenge" || strings.HasPrefix(pathOf(ref), "/mfa-challenge/"):
				next, err = f.verifyMFA(ctx, current, ref)
			case needsPhone(current):
				o["addPhoneUrl"] = ref
				if err = f.save("phone_required", o); err != nil {
					return nil, err
				}
				next, err = f.bindPhone(ctx, o, false)
			case pageType(current) == "workspace" || pathOf(ref) == "/workspace" || len(slice(payload(current["oai-client-auth-session"])["workspaces"])) > 0:
				o["current"] = current
				if err = f.save("oauth_continue", o); err != nil {
					return nil, err
				}
				return f.finishOAuth(ctx, current, o)
			default:
				return nil, errors.New("BPS_LOGIN_PAGE_INVALID: unsupported authorization step")
			}
		}
		if err != nil {
			return nil, err
		}
		current = next
		o["current"], o["pageURL"] = current, ref
		if err = f.save("bps_continue", o); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("BPS_LOGIN_PAGE_INVALID: authorization step limit exceeded")
}
