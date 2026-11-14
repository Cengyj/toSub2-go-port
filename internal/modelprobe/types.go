// Package modelprobe runs a candy reasoning check with a local answer rule.
// Its single-question result is not an intelligence quotient.
package modelprobe

import (
	"time"

	"tosub2-go-port/internal/protocol"
)

const (
	RuleVersion           = "candy-v1"
	TotalQuestions        = 1
	DefaultMinimumScore   = 1
	DefaultTimeoutSeconds = 60
	MaxTimeoutSeconds     = 300
)

type Options struct {
	Model           string
	ReasoningEffort string
	MinimumScore    int
	TimeoutSeconds  int
}

// Result is returned only for a complete answer in the required format. Error,
// unsupported, interrupted and unknown responses never become zero scores.
type Result struct {
	Model       string    `json:"model"`
	RuleVersion string    `json:"ruleVersion"`
	Verdict     string    `json:"verdict"`
	Score       int       `json:"score"`
	Total       int       `json:"total"`
	CheckedAt   time.Time `json:"checkedAt"`
	DurationMS  int64     `json:"durationMs"`
}

// Error carries only stable categories and status metadata. It never embeds
// credentials, request URLs, model text, or raw upstream diagnostics.
type Error struct {
	Code       string `json:"code"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
}

func (e *Error) Error() string {
	if e != nil {
		if message := failureMessage(e.Code); message != "" {
			return message
		}
	}
	return "模型检测未取得有效结果"
}

// Client must use the low-level transport shared with account insights. The
// authentication protocol client can replay requests and is explicitly rejected.
type Client struct{ Transport protocol.Doer }
