package cpa

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"tosub2-go-port/internal/authmaterial"
	"tosub2-go-port/internal/transport"
)

var ErrMutationOutcomeUnknown = errors.New("CPA 推送结果未确认，请先在 CPA 中核实后再重试")

type Client struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) *Client {
	cfg.UploadOptions = cfg.UploadOptions.Clone()
	return &Client{cfg: cfg, http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type RemoteError struct{ StatusCode int }

func (e *RemoteError) Error() string { return fmt.Sprintf("CPA 返回 HTTP %d", e.StatusCode) }

type uploadError struct {
	cause   error
	unknown bool
}

func (e *uploadError) Error() string {
	if e.unknown {
		return ErrMutationOutcomeUnknown.Error()
	}
	return SafeError(e.cause)
}
func (e *uploadError) Unwrap() error        { return e.cause }
func (e *uploadError) Is(target error) bool { return e.unknown && target == ErrMutationOutcomeUnknown }

// Upload performs exactly one mutation. CPA replaces a same-named file before
// registration and post-persist hooks finish, so lost/invalid responses and
// server errors cannot prove that no credentials were written.
func (c *Client) Upload(ctx context.Context, file AuthFile) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return &uploadError{cause: err}
	}
	if c == nil {
		return &uploadError{cause: ErrInvalidBaseURL}
	}
	cfg, err := validateConfig(c.cfg)
	if err != nil {
		return &uploadError{cause: err}
	}
	file, err = withUploadOptions(file, cfg.UploadOptions)
	if err != nil {
		return &uploadError{cause: err}
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+cfg.APIKey)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	request := transport.Request{Method: http.MethodPost, URL: cfg.BaseURL + "/v0/management/auth-files?" + url.Values{"name": {file.Filename}}.Encode(), Headers: headers, Body: file.Data, Timeout: cfg.Timeout, SingleAttempt: true}
	response, err := transport.DoHTTPWithRetry(ctx, c.http, request, 64<<10, 0, 0, nil)
	if err != nil {
		return &uploadError{cause: err, unknown: !transport.IsRequestDefinitelyUnsent(err)}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &uploadError{cause: &RemoteError{StatusCode: response.StatusCode}, unknown: response.StatusCode < 400 || response.StatusCode >= 500}
	}
	var receipt struct {
		Status string `json:"status"`
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Body))
	decoder.DisallowUnknownFields()
	if response.StatusCode != http.StatusOK || authmaterial.ValidateUnambiguousJSON(response.Body) != nil || decoder.Decode(&receipt) != nil || receipt.Status != "ok" {
		return &uploadError{cause: ErrMutationOutcomeUnknown, unknown: true}
	}
	return nil
}

func IsMutationOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	var upload *uploadError
	if errors.As(err, &upload) {
		return upload.unknown
	}
	return errors.Is(err, ErrMutationOutcomeUnknown)
}

// SafeError never incorporates an upstream response, request URL or secret.
func SafeError(err error) string {
	if err == nil {
		return ""
	}
	if IsMutationOutcomeUnknown(err) {
		return ErrMutationOutcomeUnknown.Error()
	}
	for _, known := range []error{ErrInvalidBaseURL, ErrInvalidAPIKey, ErrInvalidPriority, ErrInvalidWeight, ErrUnsupportedAuthorization, ErrInvalidAuthorization} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	var remote *RemoteError
	if errors.As(err, &remote) {
		return remote.Error()
	}
	if errors.Is(err, context.Canceled) {
		return "CPA 推送已取消"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "CPA 推送超时"
	}
	return "CPA 推送失败"
}
