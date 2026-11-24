package protocol

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"time"

	"tosub2-go-port/internal/transport"
)

type profileOutcome string

const (
	profileNotSent  profileOutcome = "not_sent"
	profileRejected profileOutcome = "rejected"
	profileUnknown  profileOutcome = "unknown"
)

// A profile failure is scoped to this mutation. In particular, an upstream
// session error after submission must not cause Run to start a second login
// and blindly repeat the profile write. The underlying error is retained for
// cancellation and network observation; Error never formats provider material.
type profileCompletionError struct {
	outcome profileOutcome
	code    string
	message string
	cause   error
}

func (e *profileCompletionError) Error() string { return e.code + ": " + e.message }
func (e *profileCompletionError) Unwrap() error { return e.cause }

func profileRequired(p authPayload) bool {
	return pageType(p) == "about_you" || pathOf(continuation(p)) == "/about-you"
}

func (f *Flow) profileEvent(stage, outcome, source, message string) {
	f.emitData(stage, "running", message, map[string]any{
		"profileCompletion": map[string]any{"trigger": "about_you", "source": source, "outcome": outcome},
	})
}

func (f *Flow) completeProfile(ctx context.Context, current authPayload) (authPayload, error) {
	if !profileRequired(current) {
		return current, nil
	}
	profile, source := f.Options.Profile, "provided"
	if profile == nil {
		profile, source = generatedProfile(), "generated_adult"
	}
	message := "登录返回资料未完善步骤（about_you）；将提交已提供的资料"
	if source == "generated_adult" {
		message = "登录返回资料未完善步骤（about_you）；将按现有规则自动补全成年资料"
	}
	f.profileEvent("profile_required", "required", source, message)
	stop := func(outcome profileOutcome, code, message string, cause error) (authPayload, error) {
		f.profileEvent("profile_"+string(outcome), string(outcome), source, message)
		return nil, &profileCompletionError{outcome: outcome, code: code, message: message, cause: cause}
	}
	if err := ctx.Err(); err != nil {
		return stop(profileNotSent, "PROFILE_NOT_SENT", "资料补全已停止，未提交资料", err)
	}
	h, err := f.sentinel(ctx, "oauth_create_account")
	if err != nil {
		return stop(profileNotSent, "PROFILE_NOT_SENT", "资料补全验证准备失败，未提交资料", err)
	}
	ref := continuation(current)
	if ref == "" {
		ref = f.Options.AuthBase + "/about-you"
	}
	f.profileEvent("profile_submitting", "submitting", source, "正在提交资料补全；如响应未确认，本轮不会重复提交")
	var next authPayload
	response, err := f.Client.JSON(ctx, http.MethodPost, f.Options.AuthBase+"/api/accounts/create_account", profile,
		RequestOptions{Headers: h, Origin: f.Options.AuthBase, Referer: ref, DisableChallengeReplay: true}, &next)
	if err != nil {
		if transport.IsRequestDefinitelyUnsent(err) {
			return stop(profileNotSent, "PROFILE_NOT_SENT", "传输确认资料补全请求未发送", err)
		}
		if response != nil && response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != http.StatusRequestTimeout {
			return stop(profileRejected, "PROFILE_REJECTED", "资料补全请求被拒绝，本轮已停止提交", err)
		}
		return stop(profileUnknown, "PROFILE_RESULT_UNKNOWN", "资料补全结果未确认，本轮已停止，不重复提交", err)
	}
	if profileRequired(next) {
		return stop(profileUnknown, "PROFILE_SECURITY_CHECK_REQUIRED", "资料补全响应未进入下一步，需要核对账号状态，本轮不重复提交", nil)
	}
	callback, _ := url.Parse(continuation(next))
	chat, _ := url.Parse(f.Options.ChatGPTBase)
	if pageType(next) != "external_url" || !sameOrigin(callback, chat) || callback.User != nil || callback.Fragment != "" || callback.Path != "/api/auth/callback/openai" || authenticationReceiptRejected(next) {
		return stop(profileUnknown, "PROFILE_RESULT_INVALID", "资料补全响应未确认预期回调，本轮不重复提交", nil)
	}
	f.profileEvent("profile_completed", "succeeded", source, "资料补全已确认，正在继续本次登录")
	return next, nil
}

func isProfileCompletionFailure(err error) bool {
	var failure *profileCompletionError
	return errors.As(err, &failure)
}
func generatedProfile() map[string]any {
	first := []string{"Alex", "Avery", "Blake", "Cameron", "Casey", "Drew", "Emerson", "Hayden", "Jamie", "Jordan", "Logan", "Morgan", "Parker", "Quinn", "Reese", "Taylor"}
	last := []string{"Adams", "Baker", "Brooks", "Carter", "Clark", "Collins", "Cooper", "Evans", "Foster", "Gray", "Hall", "Hayes", "Morgan", "Perry", "Reed", "Walker"}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	latest := today.AddDate(-20, 0, 0)
	earliest := today.AddDate(-51, 0, 1)
	span := int(latest.Sub(earliest).Hours() / 24)
	return map[string]any{"name": first[profileRandom(len(first))] + " " + last[profileRandom(len(last))], "birthdate": earliest.AddDate(0, 0, profileRandom(span+1)).Format("2006-01-02")}
}
func profileRandom(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}
