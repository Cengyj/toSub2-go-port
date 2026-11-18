package protocol

import (
	"errors"
	"strings"
)

var ErrResetStorageBlocked = errors.New("TOTP_RESET_STORAGE_BLOCKED: 操作证据尚未可靠保存")

type resetCapture struct {
	path                string
	out                 map[string]any
	enrollmentStep      string
	verifiedLoginFactor string // transient proof from this reset's own MFA challenge
}

// A verified manual challenge proves possession without binding a seed in the
// vault. A conflict GET alone cannot identify the factor the user controls.
func (f *Flow) permitsResetTarget(id string, requireBinding bool) bool {
	if id == "" {
		return false
	}
	stored, live := f.Options.TOTPFactorID, ""
	if f.resetCapture != nil {
		live = f.resetCapture.verifiedLoginFactor
	}
	return (stored == "" || stored == id) && (live == "" || live == id) && (!requireBinding || stored == id || live == id)
}

func (f *Flow) attachResetCapture(path string, out map[string]any) (func(), error) {
	if f == nil || f.Client == nil || f.resetCapture != nil || strings.TrimSpace(str(out["operation_id"])) == "" || !strings.EqualFold(strings.TrimSpace(f.Options.Email), strings.TrimSpace(str(out["email"]))) {
		return nil, errors.New("TOTP_RESET_OPERATION_CONFLICT: 当前操作绑定不一致")
	}
	f.resetCapture = &resetCapture{path: path, out: out}
	return func() { f.resetCapture = nil }, nil
}
