package protocol

import (
	"context"
	"errors"
	"fmt"
	"net/url"
)

var ErrSessionExpired = errors.New("authentication session expired")

func expiredSession(code string) error {
	return fmt.Errorf("%s: %w", code, ErrSessionExpired)
}

// Recovery is restricted to explicit session errors or a confirmed redirect
// to a login page. Missing markup, checkpoints, transport failures, challenges,
// rejected passwords/codes and permission errors are not expiry signals.
func isExpiredSession(err error) bool {
	if isProfileCompletionFailure(err) {
		return false
	}
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

func (f *Flow) startLoginOperation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Client.ResetSession(ctx); err != nil {
		return err
	}
	f.checkpoint, f.web = nil, nil
	f.verifiedPassword, f.deviceID = "", ""
	return nil
}

func (f *Flow) resetExpiredSession(ctx context.Context) error {
	if err := f.startLoginOperation(ctx); err != nil {
		return err
	}
	f.emit("auth_expired", "running", "Current authentication expired; starting one fresh login")
	return nil
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
