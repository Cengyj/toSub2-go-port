package protocol

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

var ErrSessionExpired = errors.New("authentication session expired")

func expiredSession(code string) error {
	return fmt.Errorf("%s: %w", code, ErrSessionExpired)
}

// Recovery is restricted to explicit session errors or a confirmed redirect
// to a login page. Missing markup, checkpoints, transport failures, challenges,
// rejected passwords/codes and permission errors are not expiry signals.
func isExpiredSession(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrSessionExpired) {
		return true
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	switch httpErr.StatusCode {
	case 400, 401, 403, 409, 422:
		switch httpErr.Code {
		case "session_expired", "session_invalid", "invalid_session", "invalid_state", "login_required":
			return true
		}
		// A legacy endpoint can describe expiry without a machine-readable
		// code. Keep explicit password/account/permission classifications in
		// control when present, and never retain the remote error text.
		return httpErr.Code == "" && httpErr.sessionExpiredText
	}
	return false
}

func (f *Flow) isLoginURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	auth, _ := url.Parse(f.Options.AuthBase)
	chat, _ := url.Parse(f.Options.ChatGPTBase)
	if sameOrigin(u, auth) {
		switch u.Path {
		case "/log-in", "/log-in/password", "/log-in-or-create-account":
			return true
		}
	}
	return sameOrigin(u, chat) && u.Path == "/auth/login"
}

func (f *Flow) resetExpiredSession(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.Options.CheckpointPath != "" {
		if err := RemoveCheckpoint(f.Options.CheckpointPath); err != nil {
			return err
		}
	}
	if err := f.Client.ResetSession(ctx); err != nil {
		return err
	}
	f.checkpoint, f.web = nil, nil
	f.verifiedPassword = ""
	// Keep the existing device ID to match a bound Chromium context even if
	// an older checkpoint did not persist an oai-did cookie.
	f.emit("auth_expired", "running", "Saved authentication expired; signing in once again")
	return nil
}

func (f *Flow) canReuseLogin() bool {
	if f.Client.Jar.Value("__Secure-next-auth.session-token", f.Options.ChatGPTBase+"/") != "" {
		return true
	}
	if f.checkpoint == nil || strings.HasPrefix(f.checkpoint.Stage, "web_") {
		return false
	}
	switch f.checkpoint.Stage {
	case "email_verified", "phone_required", "phone_otp", "oauth_continue", "callback_ready", tokenCheckpointStage:
		return true
	}
	return false
}

func (f *Flow) ensureDeviceID() {
	if cookieDevice := f.Client.Jar.Value("oai-did", f.Options.ChatGPTBase+"/"); cookieDevice != "" {
		f.deviceID = cookieDevice
	}
	if f.deviceID == "" {
		f.deviceID = randomUUID()
	}
	if f.web == nil {
		f.web = map[string]any{}
	}
	f.web["deviceId"] = f.deviceID
}

func (f *Flow) prepareEnrollmentSession(ctx context.Context) error {
	if !f.canReuseLogin() {
		_, err := f.LoginWeb(ctx)
		return err
	}
	f.ensureDeviceID()
	return nil
}
