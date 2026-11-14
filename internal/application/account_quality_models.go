package application

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/protocol"
)

// AccountModels returns the selected account's fresh information catalog. The
// ordinary query lease prevents an overlapping login or token rotation; no
// quality setting, result, account operation, grant or mapping is changed.
func (c *AccountQualityController) AccountModels(ctx context.Context, id string) (protocol.AccountModelCatalog, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var recorder *jobs.AccountRequestRecorder
	var observationGrant *AccountInsightGrant
	fail := func(code string) (protocol.AccountModelCatalog, error) {
		if recorder != nil && ctx.Err() == nil && code != "canceled" && code != "busy" && code != "superseded" {
			observation := jobs.AccountRequestObservation{Status: "failed", ErrorCode: code}
			if observationGrant != nil {
				completeGrantRequest(observationGrant, recorder, observation)
			} else {
				_ = recorder.Complete(observation)
			}
		}
		return protocol.AccountModelCatalog{}, &protocol.AccountModelsError{Code: code}
	}
	if c == nil || c.manager == nil {
		return fail("transport_unavailable")
	}
	job, ok := c.manager.Get(id)
	if !ok {
		return fail("not_found")
	}
	recorder = c.manager.BeginAccountRequest(id, "models")
	if !job.ResultSaved {
		return fail("authorization_unavailable")
	}
	grant, err := LoadAccountInsightGrant(ctx, c.cfg.OutputRoot, c.manager, id)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return fail("canceled")
		case errors.Is(err, context.DeadlineExceeded):
			return fail("timeout")
		case errors.Is(err, jobs.ErrAccountQueryBusy):
			return fail("busy")
		case errors.Is(err, jobs.ErrAccountQuerySuperseded):
			return fail("superseded")
		case errors.Is(err, ErrInsightProxyUnavailable):
			return fail("proxy_selection_required")
		case errors.Is(err, ErrInsightGrantIdentity):
			return fail("invalid_credentials")
		case errors.Is(err, ErrInsightRecoveryPending), errors.Is(err, jobs.ErrTOTPRecoveryPending):
			return fail("recovery_pending")
		default:
			return fail("authorization_unavailable")
		}
	}
	observationGrant = grant
	// Bind network diagnostics to the acquired query reservation as well as
	// the account operation scope captured by preflight diagnostics.
	recorder = c.manager.BeginAccountRequest(id, "models")
	defer grant.Release()
	client, closeTransport, err := c.newProbeClient(grant)
	if err != nil {
		var failure *modelprobe.Error
		if errors.As(err, &failure) {
			return fail(failure.Code)
		}
		return fail("transport_unavailable")
	}
	defer closeTransport()
	reader := protocol.InsightsClient{Transport: client.Transport}
	catalog, err := reader.Models(grant.Context(), grant.Credentials)
	if err != nil {
		var failure *protocol.AccountModelsError
		if errors.As(err, &failure) && failure.Code != "canceled" && failure.Code != "superseded" {
			completeGrantRequest(grant, recorder, jobs.AccountRequestObservation{Status: "failed", HTTPStatus: failure.HTTPStatus, ErrorCode: failure.Code})
		}
		return protocol.AccountModelCatalog{}, err
	}
	if err := grant.Validate(); err != nil {
		return fail("superseded")
	}
	completeGrantRequest(grant, recorder, jobs.AccountRequestObservation{Status: "success", HTTPStatus: http.StatusOK})
	catalog.Models = probeModelChoices(catalog.Models)
	catalog.SuggestedModels = probeModelChoices(catalog.SuggestedModels)
	if len(catalog.Models) == 0 {
		catalog.Message = "该账号当前未提供可选探针模型"
	}
	return catalog, nil
}

func probeModelChoices(models []protocol.AccountModel) []protocol.AccountModel {
	if models == nil {
		return nil
	}
	choices := make([]protocol.AccountModel, 0, len(models))
	for _, model := range models {
		switch strings.ToLower(model.ID) {
		case "gpt-reserve", "gpt-6-chat-thinking", "codex-auto-review":
			continue
		}
		choices = append(choices, model)
	}
	return choices
}
