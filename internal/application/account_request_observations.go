package application

import (
	"context"
	"errors"
	"net/http"

	"tosub2-go-port/internal/accountquality"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/protocol"
)

// Account request observations are display-only diagnostics. A failed local
// write must never change the request result or authorize a retry.
func beginGrantRequest(grant *AccountInsightGrant, source string) *jobs.AccountRequestRecorder {
	if grant == nil || grant.manager == nil {
		return nil
	}
	return grant.manager.BeginAccountRequest(grant.id, source)
}

func completeGrantRequest(grant *AccountInsightGrant, recorder *jobs.AccountRequestRecorder, observations ...jobs.AccountRequestObservation) {
	if recorder == nil || grant == nil || grant.Validate() != nil {
		return
	}
	_ = recorder.Complete(observations...)
}

func requestCanceledOrSuperseded(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, jobs.ErrAccountQuerySuperseded) || errors.Is(err, jobs.ErrAccountQueryBusy) || errors.Is(err, accountquality.ErrSuperseded) {
		return true
	}
	var gate *accountquality.GateError
	if errors.As(err, &gate) && (gate.Code == "superseded" || gate.Code == "busy") {
		return true
	}
	var failure *modelprobe.Error
	if errors.As(err, &failure) && failure != nil {
		return failure.Code == "canceled" || failure.Code == "cancelled" || failure.Code == "superseded"
	}
	return false
}

func probeRequestObservation(err error) (jobs.AccountRequestObservation, bool) {
	if requestCanceledOrSuperseded(err) {
		return jobs.AccountRequestObservation{}, false
	}
	if err == nil {
		// Both probe clients return success only after an HTTP 200 stream and
		// the complete result have passed their format validation.
		return jobs.AccountRequestObservation{Status: "success", HTTPStatus: http.StatusOK}, true
	}
	diagnostic := modelprobe.DiagnoseFailure(err)
	return jobs.AccountRequestObservation{Status: "failed", HTTPStatus: diagnostic.HTTPStatus, ErrorCode: diagnostic.Code}, true
}

func completeProbeRequest(grant *AccountInsightGrant, recorder *jobs.AccountRequestRecorder, err error) {
	if observation, ok := probeRequestObservation(err); ok {
		completeGrantRequest(grant, recorder, observation)
	}
}

func subscriptionRequestObservations(result protocol.InsightResult, confirmed bool) []jobs.AccountRequestObservation {
	observations := make([]jobs.AccountRequestObservation, 0, len(result.Errors))
	for _, failure := range result.Errors {
		switch failure.Code {
		case "canceled", "cancelled", "superseded", "query_superseded":
			return nil
		}
		observations = append(observations, jobs.AccountRequestObservation{Status: "failed", HTTPStatus: failure.HTTPStatus, ErrorCode: failure.Code})
	}
	if len(observations) != 0 {
		return observations
	}
	if confirmed {
		return []jobs.AccountRequestObservation{{Status: "success", HTTPStatus: http.StatusOK}}
	}
	observation := jobs.AccountRequestObservation{Status: "failed", ErrorCode: "subscription_unconfirmed"}
	if result.Subscription != nil {
		// A parsed subscription is produced only after a received HTTP 200;
		// unknown plan semantics do not erase the observed transport status.
		observation.HTTPStatus = http.StatusOK
	}
	return []jobs.AccountRequestObservation{observation}
}
