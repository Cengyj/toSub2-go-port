package accountquality

import (
	"errors"
	"time"

	"tosub2-go-port/internal/modelprobe"
)

func cloneHTTPObservation(observation *HTTPObservation) *HTTPObservation {
	if observation == nil {
		return nil
	}
	copy := *observation
	return &copy
}

func knownLegacyHTTPFailure(code string) bool {
	switch code {
	case "unauthorized", "forbidden", "rate_limited":
		return true
	default:
		return false
	}
}

// Only a complete valid answer clears an older transport failure. A malformed
// HTTP 200, a missing response, or a cancellation is not a new observation.
func completedHTTPObservation(result modelprobe.Result, runErr error, terminalStatus, model string, finishedAt time.Time) *HTTPObservation {
	if runErr == nil && terminalStatus == "success" {
		return &HTTPObservation{Status: "success", HTTPStatus: 200, Message: "探针请求已完整完成", FinishedAt: finishedAt, Source: "candy", Model: result.Model}
	}
	var failure *modelprobe.Error
	if terminalStatus != "failed" || !errors.As(runErr, &failure) || failure == nil || failure.HTTPStatus < 300 || failure.HTTPStatus > 599 {
		return nil
	}
	diagnostic := modelprobe.DiagnoseFailure(runErr)
	return &HTTPObservation{Status: "failed", HTTPStatus: failure.HTTPStatus, ErrorCode: diagnostic.Code, Message: diagnostic.Message, FinishedAt: finishedAt, Source: "candy", Model: model}
}

// Recreate a bounded public message on load rather than trusting stored free
// text. The independent authorization revision is never inferred or replaced.
func safeHTTPObservation(observation *HTTPObservation) *HTTPObservation {
	if observation == nil || observation.Source != "candy" || observation.FinishedAt.IsZero() {
		return nil
	}
	copy := *observation
	switch {
	case copy.Status == "success" && copy.HTTPStatus == 200:
		copy.ErrorCode, copy.Message = "", "探针请求已完整完成"
	case copy.Status == "failed" && (copy.HTTPStatus >= 300 && copy.HTTPStatus <= 599 || copy.HTTPStatus == 0 && knownLegacyHTTPFailure(copy.ErrorCode)):
		diagnostic := modelprobe.DiagnoseFailure(&modelprobe.Error{Code: copy.ErrorCode, HTTPStatus: copy.HTTPStatus})
		copy.ErrorCode, copy.Message = diagnostic.Code, diagnostic.Message
	default:
		return nil
	}
	return &copy
}

// Older rows have no independent observation. Backfill only terminal evidence
// bound to the currently known authorization. Starting a new attempt changes
// row.AccountRevision, so newer observations always retain their own revision.
func backfillHTTPObservation(row *storedRow, account Account, active bool, now time.Time) {
	if row.HTTPObservation != nil {
		row.HTTPObservation = safeHTTPObservation(row.HTTPObservation)
		return
	}
	if !active || row.AccountRevision == "" || row.AccountRevision != account.Revision {
		return
	}
	attempt := row.Snapshot.LastAttempt
	if attempt.FinishedAt == nil || attempt.FinishedAt.IsZero() || attempt.FinishedAt.After(now.Add(time.Minute)) {
		return
	}
	var observation *HTTPObservation
	switch {
	case attempt.Status == "success":
		observation = &HTTPObservation{Status: "success", HTTPStatus: 200, Message: "探针请求已完整完成", FinishedAt: *attempt.FinishedAt, Source: "candy"}
	case attempt.Status == "failed" && (attempt.HTTPStatus >= 300 && attempt.HTTPStatus <= 599 || attempt.HTTPStatus == 0 && knownLegacyHTTPFailure(attempt.ErrorCode)):
		diagnostic := modelprobe.DiagnoseFailure(&modelprobe.Error{Code: attempt.ErrorCode, HTTPStatus: attempt.HTTPStatus})
		observation = &HTTPObservation{Status: "failed", HTTPStatus: attempt.HTTPStatus, ErrorCode: diagnostic.Code, Message: diagnostic.Message, FinishedAt: *attempt.FinishedAt, Source: "candy"}
	}
	if observation != nil {
		row.HTTPObservation, row.HTTPObservationRevision = observation, row.AccountRevision
	}
}
