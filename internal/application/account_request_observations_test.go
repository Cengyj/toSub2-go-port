package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func requestObservations(t *testing.T, f *insightGrantFixture, source string) []jobs.AccountRequestObservation {
	t.Helper()
	job, ok := f.manager.Get(f.job.ID)
	if !ok {
		t.Fatal("synthetic account missing")
	}
	var out []jobs.AccountRequestObservation
	for _, observation := range job.AccountRequestObservations {
		if observation.Source == source {
			out = append(out, observation)
		}
	}
	return out
}

func requireRequestObservation(t *testing.T, f *insightGrantFixture, source, state string, status int) jobs.AccountRequestObservation {
	t.Helper()
	observations := requestObservations(t, f, source)
	if len(observations) != 1 || observations[0].Status != state || observations[0].HTTPStatus != status || observations[0].FinishedAt.IsZero() {
		t.Fatalf("unexpected %s request observations: %+v", source, observations)
	}
	return observations[0]
}

func seedRequestFailure(t *testing.T, f *insightGrantFixture, source string, status int) {
	t.Helper()
	if err := f.manager.BeginAccountRequest(f.job.ID, source).Complete(jobs.AccountRequestObservation{Status: "failed", HTTPStatus: status, ErrorCode: "upstream_error"}); err != nil {
		t.Fatal(err)
	}
}

func TestAccountRequestSubscriptionFailureAndSuccessStaySourceScoped(t *testing.T) {
	f := subscriptionRefreshFixture(t, "free")
	seedRequestFailure(t, f, "models", http.StatusUnauthorized)
	before, _ := f.manager.Get(f.job.ID)
	grant := f.load(t)
	client := &protocol.InsightsClient{Transport: subscriptionRefreshDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		return &transport.Response{StatusCode: http.StatusForbidden, Body: []byte("synthetic-private-cookie")}, nil
	})}
	if _, _, err := RefreshAccountSubscription(t.Context(), grant, client, protocol.InsightsOptions{}); err == nil {
		t.Fatal("synthetic denied subscription succeeded")
	}
	failed := requireRequestObservation(t, f, "subscription", "failed", http.StatusForbidden)
	if failed.ErrorCode == "" || strings.Contains(failed.Message, "synthetic-private") {
		t.Fatal("subscription diagnostics lost category or leaked upstream content")
	}
	grant.Release()
	grant = f.load(t)
	client.Transport = subscriptionRefreshDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		return subscriptionRefreshResponse(t, "plus", "2030-01-01T00:00:00Z"), nil
	})
	if _, _, err := RefreshAccountSubscription(t.Context(), grant, client, protocol.InsightsOptions{}); err != nil {
		t.Fatal(err)
	}
	requireRequestObservation(t, f, "subscription", "success", http.StatusOK)
	requireRequestObservation(t, f, "models", "failed", http.StatusUnauthorized)
	after, _ := f.manager.Get(f.job.ID)
	if after.Attempts != before.Attempts || after.Operation != before.Operation || !after.LastOperationAt.Equal(before.LastOperationAt) {
		t.Fatal("request display diagnostics changed account operation history")
	}
}

func TestAccountRequestSubscriptionKeepsPartialEndpointFailure(t *testing.T) {
	f := subscriptionRefreshFixture(t, "free")
	client := &protocol.InsightsClient{Transport: subscriptionRefreshDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		if strings.Contains(r.URL, "accounts/check") {
			return subscriptionRefreshResponse(t, "plus", nil), nil
		}
		return &transport.Response{StatusCode: http.StatusServiceUnavailable}, nil
	})}
	updated, _, err := RefreshAccountSubscription(t.Context(), f.load(t), client, protocol.InsightsOptions{})
	if err != nil || updated.PlanType != "plus" {
		t.Fatal("safe plan observation was lost", err)
	}
	requireRequestObservation(t, f, "subscription", "failed", http.StatusServiceUnavailable)
}

func TestAccountRequestModelsPreserveActualHTTPAndZeroForNoResponse(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		status   int
		response *transport.Response
		err      error
	}{
		{"denied", 403, &transport.Response{StatusCode: 403, Body: []byte("synthetic-private-response")}, nil},
		{"malformed-success", 200, &transport.Response{StatusCode: 200, Body: []byte("synthetic-private-response")}, nil},
		{"no-response", 0, nil, errors.New("synthetic-private-network-detail")},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
			observationTransport(c, qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				return scenario.response, scenario.err
			}))
			if _, err := c.AccountModels(t.Context(), f.job.ID); err == nil {
				t.Fatal("synthetic failed model catalog succeeded")
			}
			observation := requireRequestObservation(t, f, "models", "failed", scenario.status)
			data, _ := json.Marshal(observation)
			if strings.Contains(string(data), "synthetic-private") || observation.ErrorCode == "" {
				t.Fatal("unsafe or unclassified model diagnostics")
			}
			observationTransport(c, qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				return qualityModelResponse(jobs.AuthorizationMethodChatGPT), nil
			}))
			if _, err := c.AccountModels(t.Context(), f.job.ID); err != nil {
				t.Fatal(err)
			}
			requireRequestObservation(t, f, "models", "success", http.StatusOK)
		})
	}
}

func TestAccountRequestModelsCancellationAndSupersessionPreserveFailure(t *testing.T) {
	for _, scenario := range []string{"cancel", "replace-grant"} {
		t.Run(scenario, func(t *testing.T) {
			f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
			seedRequestFailure(t, f, "models", http.StatusTooManyRequests)
			before := requestObservations(t, f, "models")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			observationTransport(c, qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				if scenario == "cancel" {
					cancel()
				} else {
					f.account["credentials"].(map[string]any)["access_token"] = "synthetic-new-grant"
					f.write(t)
				}
				return qualityModelResponse(jobs.AuthorizationMethodChatGPT), nil
			}))
			if _, err := c.AccountModels(ctx, f.job.ID); err == nil {
				t.Fatal("canceled or superseded catalog was adopted")
			}
			if !reflect.DeepEqual(before, requestObservations(t, f, "models")) {
				t.Fatal("canceled or superseded request erased prior failure")
			}
		})
	}
}

func TestAccountRequestCandyAndPelicanRemainIndependent(t *testing.T) {
	f, c := newQualityControllerFixture(t, jobs.AuthorizationMethodChatGPT)
	c.StartManualProbes(t.Context())
	enableQualityFixture(t, c, nil)
	observationTransport(c, qualityDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		return &transport.Response{StatusCode: http.StatusForbidden}, nil
	}))
	runObservationManual(t, c, f.job.ID, "account-source-pelican-01")
	requireRequestObservation(t, f, "pelican", "failed", http.StatusForbidden)
	observationTransport(c, qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		return qualitySyntheticResponse(r, true), nil
	}))
	grant := f.load(t)
	run, closeTransport, err := c.probe(grant)
	if err != nil {
		t.Fatal(err)
	}
	_, err = run(t.Context(), modelprobe.Options{Model: "synthetic-model", MinimumScore: 1, TimeoutSeconds: 10})
	closeTransport()
	grant.Release()
	if err != nil {
		t.Fatal(err)
	}
	requireRequestObservation(t, f, "candy", "success", http.StatusOK)
	requireRequestObservation(t, f, "pelican", "failed", http.StatusForbidden)
	observationTransport(c, qualityDoer(func(_ context.Context, r transport.Request) (*transport.Response, error) {
		return manualResponse(r, manualDrawing), nil
	}))
	runObservationManual(t, c, f.job.ID, "account-source-pelican-02")
	requireRequestObservation(t, f, "pelican", "success", http.StatusOK)
	requireRequestObservation(t, f, "candy", "success", http.StatusOK)
}

func TestAccountRequestSubscriptionPreservesEveryReturnedFailure(t *testing.T) {
	result := protocol.InsightResult{Errors: []protocol.InsightError{{HTTPStatus: 401, Code: "authorization_rejected"}, {HTTPStatus: 503, Code: "upstream_error"}}}
	observations := subscriptionRequestObservations(result, false)
	if len(observations) != 2 || observations[0].HTTPStatus != 401 || observations[1].HTTPStatus != 503 {
		t.Fatalf("partial query failures were collapsed: %+v", observations)
	}
	unconfirmed := subscriptionRequestObservations(protocol.InsightResult{Subscription: &protocol.InsightSubscription{PlanType: "unknown"}}, false)
	if len(unconfirmed) != 1 || unconfirmed[0].Status != "failed" || unconfirmed[0].HTTPStatus != http.StatusOK {
		t.Fatal("parsed but unconfirmed subscription lost received HTTP 200")
	}
}
