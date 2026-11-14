package protocol

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/transport"
)

func requireModelCatalogError(t *testing.T, err error, code string, status int) {
	t.Helper()
	var failure *AccountModelsError
	if !errors.As(err, &failure) || failure.Code != code || failure.HTTPStatus != status {
		t.Fatalf("unexpected model catalog failure: %v", err)
	}
	for _, private := range []string{"synthetic-access-secret", "private upstream diagnostic", "https://"} {
		if strings.Contains(failure.Error(), private) {
			t.Fatal("catalog error exposed private diagnostics")
		}
	}
}

func TestAccountModelsUseOneOfficialReadForSelectedAuthorization(t *testing.T) {
	for _, method := range []string{AuthorizationMethodChatGPT, AuthorizationMethodBPS} {
		t.Run(method, func(t *testing.T) {
			creds := insightTestCredentials()
			creds.AuthorizationMethod = method
			calls := 0
			client := &InsightsClient{Transport: insightFixtureDoer(func(ctx context.Context, r transport.Request) (*transport.Response, error) {
				calls++
				if r.Method != http.MethodGet || len(r.Body) != 0 || !r.SingleAttempt || r.RetryReadOnly || r.RetryBeforeSend || r.MaxResponseBytes != insightMaxBodyBytes {
					t.Fatal("model read could infer or retry")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 15*time.Second || r.Timeout <= 0 || r.Timeout > 15*time.Second {
					t.Fatal("model read has no bounded context")
				}
				if r.Headers.Get("Authorization") != "Bearer "+creds.AccessToken || r.Headers.Get("Chatgpt-Account-Id") != creds.AccountID || r.Headers.Get("Cookie") != "" {
					t.Fatal("model read lost selected grant identity")
				}
				if method == AuthorizationMethodBPS {
					if r.URL != insightBPSAccessURL || r.Headers.Get("X-Openai-Account-Id") != creds.AccountID || r.Headers.Get("X-Basispoints-Auth-Mode") != "chatgpt" || r.Headers.Get("Originator") != "" {
						t.Fatal("model read mixed BPS and Codex routes")
					}
					return insightResponse(200, `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-synthetic","label":"Synthetic","efforts":[{"value":"low"},{"value":"high"}]}]}}`), nil
				}
				if r.URL != accountCodexModelsURL || r.Headers.Get("Version") != accountCodexModelsVersion || r.Headers.Get("Originator") != "codex_cli_rs" || !strings.Contains(r.Headers.Get("User-Agent"), "/"+accountCodexModelsVersion+" ") || r.Headers.Get("X-Basispoints-Auth-Mode") != "" {
					t.Fatal("model read lost paired Codex manifest version")
				}
				return insightResponse(200, `{"models":[{"slug":"gpt-synthetic","display_name":"Synthetic","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]}]}`), nil
			})}
			catalog, err := client.Models(t.Context(), creds)
			if err != nil || calls != 1 || catalog.FetchedAt.IsZero() || len(catalog.Models) != 1 || catalog.Models[0].ID != "gpt-synthetic" || catalog.Models[0].DisplayName != "Synthetic" || !reflect.DeepEqual(catalog.Models[0].ReasoningEfforts, []string{"low", "high"}) {
				t.Fatalf("selected catalog unavailable: calls=%d catalog=%+v err=%v", calls, catalog, err)
			}
			wantSource := "codex-models"
			if method == AuthorizationMethodBPS {
				wantSource = "bps-access"
			}
			if catalog.Source != wantSource {
				t.Fatal("catalog source did not identify authoritative endpoint")
			}
		})
	}
}

func TestAccountModelsFilterAuthoritativePermissionsWithoutLocalFallback(t *testing.T) {
	for _, tc := range []struct {
		method, body string
		want         []string
	}{
		{AuthorizationMethodBPS, `{"allowed":true,"model_catalog":{"restricted_models":["gpt-restricted"],"models":[{"id":"gpt-allowed"},{"id":"gpt-restricted"},{"id":"gpt-unavailable","available":false},{"id":"gpt-disabled","enabled":false},{"id":"gpt-hidden","model_picker_enabled":false},{"id":"gpt-policy","policy":{"state":"disabled"}},{"id":"unsafe/path"}]}}`, []string{"gpt-allowed"}},
		{AuthorizationMethodChatGPT, `{"models":[{"slug":"gpt-allowed"},{"id":"other:allowed","display_name":" Other "},{"slug":"gpt-hidden","visibility":"hide"},{"slug":"gpt-disabled","enabled":false},{"slug":"gpt-unavailable","available":false},{"slug":"gpt-picker","model_picker_enabled":false},{"slug":"unsafe/path"}]}`, []string{"gpt-allowed", "other:allowed", "gpt-hidden", "gpt-picker"}},
		{AuthorizationMethodBPS, `{"allowed":true,"model_catalog":{"models":[]}}`, []string{}},
		{AuthorizationMethodChatGPT, `{"models":[]}`, []string{}},
	} {
		t.Run(tc.method+tc.body[:12], func(t *testing.T) {
			creds := insightTestCredentials()
			creds.AuthorizationMethod = tc.method
			client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				return insightResponse(200, tc.body), nil
			})}
			catalog, err := client.Models(t.Context(), creds)
			if err != nil || catalog.Models == nil {
				t.Fatal("authoritative catalog rejected", err)
			}
			got := []string{}
			for _, model := range catalog.Models {
				got = append(got, model.ID)
			}
			if !reflect.DeepEqual(got, tc.want) || len(got) == 0 && catalog.Message == "" {
				t.Fatalf("permissions or empty catalog fabricated: %+v", catalog)
			}
		})
	}
}

func TestAccountModelsCodexPickerVisibilityIsNotCallPermission(t *testing.T) {
	client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		return insightResponse(200, `{"models":[
			{"slug":"gpt-next","display_name":"GPT Next","visibility":"hide","model_picker_enabled":false,"supported_reasoning_levels":[{"effort":"high"},{"effort":"xhigh"}]},
			{"slug":"gpt-preview","visibility":"preview"},
			{"slug":"gpt-denied","visibility":"hide","available":false},
			{"slug":"gpt-disabled","model_picker_enabled":false,"enabled":false}
		]}`), nil
	})}
	catalog, err := client.Models(t.Context(), insightTestCredentials())
	if err != nil || len(catalog.Models) != 2 {
		t.Fatalf("Codex picker metadata hid callable entries: models=%d err=%v", len(catalog.Models), err)
	}
	if catalog.Models[0].ID != "gpt-next" || catalog.Models[0].DisplayName != "GPT Next" || !reflect.DeepEqual(catalog.Models[0].ReasoningEfforts, []string{"high", "xhigh"}) || catalog.Models[1].ID != "gpt-preview" {
		t.Fatal("retained Codex models lost names, order or declared reasoning efforts")
	}
}

func TestAccountModelsRejectMalformedAndMismatchedCatalogs(t *testing.T) {
	for _, tc := range []struct{ method, body, code string }{
		{AuthorizationMethodBPS, `{"allowed":false}`, "forbidden"},
		{AuthorizationMethodBPS, `{"allowed":null}`, "invalid_response"},
		{AuthorizationMethodBPS, `{"allowed":true}`, "invalid_response"},
		{AuthorizationMethodBPS, `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-a"},{"id":"gpt-a"}]}}`, "invalid_response"},
		{AuthorizationMethodChatGPT, `{"models":null}`, "invalid_response"},
		{AuthorizationMethodChatGPT, `{"models":[] ,"models":[]}`, "invalid_response"},
		{AuthorizationMethodChatGPT, `{"models":[{"slug":"gpt-a"},{"slug":"gpt-a"}]}`, "invalid_response"},
		{AuthorizationMethodChatGPT, `{"models":[{"slug":"gpt-a","enabled":"yes"}]}`, "invalid_response"},
		{AuthorizationMethodChatGPT, `{"models":[{"slug":"gpt-a","supported_reasoning_levels":[null]}]}`, "invalid_response"},
		{AuthorizationMethodChatGPT, `{"account_id":"other","models":[]}`, "identity_mismatch"},
		{AuthorizationMethodBPS, `{"account_id":"other","allowed":true,"model_catalog":{"models":[]}}`, "identity_mismatch"},
	} {
		t.Run(tc.method+tc.body[:12], func(t *testing.T) {
			creds := insightTestCredentials()
			creds.AuthorizationMethod = tc.method
			client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				return insightResponse(200, tc.body), nil
			})}
			_, err := client.Models(t.Context(), creds)
			// A business denial preserves the received HTTP 200, distinct from
			// the genuine HTTP 403 tested by BoundFailuresAndNeverFallback.
			requireModelCatalogError(t, err, tc.code, http.StatusOK)
		})
	}
}

func TestAccountModelsBoundFailuresAndNeverFallback(t *testing.T) {
	redirect, _ := url.Parse("https://example.test/private")
	for _, tc := range []struct {
		name, code string
		response   *transport.Response
		err        error
		status     int
	}{
		{"unauthorized", "unauthorized", insightResponse(401, "private upstream diagnostic"), errors.New("private upstream diagnostic"), 401},
		{"forbidden", "forbidden", insightResponse(403, "private upstream diagnostic"), nil, 403},
		{"limited", "rate_limited", insightResponse(429, "private upstream diagnostic"), nil, 429},
		{"upstream", "upstream_error", insightResponse(503, "private upstream diagnostic"), nil, 503},
		{"missing-status", "upstream_error", insightResponse(0, `{"models":[]}`), nil, 0},
		{"redirect-status", "redirect_blocked", insightResponse(302, ""), nil, 302},
		{"redirect-followed", "redirect_blocked", &transport.Response{StatusCode: 200, URL: redirect}, nil, 200},
		{"html", "invalid_response", insightResponse(200, "<html>private upstream diagnostic</html>"), nil, 200},
		{"body-limit", "response_too_large", insightResponse(200, strings.Repeat("a", insightMaxBodyBytes+1)), nil, 200},
		{"transport-limit", "response_too_large", nil, transport.ErrResponseTooLarge, 0},
		{"network", "network_error", nil, errors.New("private upstream diagnostic"), 0},
		{"empty", "network_error", nil, nil, 0},
		{"canceled", "canceled", nil, context.Canceled, 0},
		{"timeout", "timeout", nil, context.DeadlineExceeded, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				calls++
				return tc.response, tc.err
			})}
			_, err := client.Models(t.Context(), insightTestCredentials())
			requireModelCatalogError(t, err, tc.code, tc.status)
			if calls != 1 {
				t.Fatal("failed catalog was retried or replaced")
			}
		})
	}
}

func TestAccountModelsValidateCredentialsAndCancellationBeforeRead(t *testing.T) {
	calls := 0
	client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) { calls++; return nil, nil })}
	for _, field := range []string{"token", "account", "method"} {
		creds := insightTestCredentials()
		switch field {
		case "token":
			creds.AccessToken = "invalid\r\nsecret"
		case "account":
			creds.AccountID = ""
		case "method":
			creds.AuthorizationMethod = "unknown"
		}
		_, err := client.Models(t.Context(), creds)
		requireModelCatalogError(t, err, "invalid_credentials", 0)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := client.Models(ctx, insightTestCredentials())
	requireModelCatalogError(t, err, "canceled", 0)
	if calls != 0 {
		t.Fatal("invalid or canceled catalog dispatched a request")
	}
	client.Transport = &Client{}
	_, err = client.Models(t.Context(), insightTestCredentials())
	requireModelCatalogError(t, err, "unsafe_transport", 0)
	client.Transport = nil
	_, err = client.Models(t.Context(), insightTestCredentials())
	requireModelCatalogError(t, err, "transport_unavailable", 0)
}

func TestAccountModelsCodexEffortsRemainAuthoritativeAndDeduplicated(t *testing.T) {
	models, err := parseCodexAccountModels([]byte(`{"models":[{"slug":"gpt-a","supported_reasoning_levels":[{"effort":"high"},{"effort":"ultra"},"low"],"reasoning_efforts":["high","none","unrecognized"]}]}`), insightTestCredentials())
	if err != nil || len(models) != 1 || !reflect.DeepEqual(models[0].ReasoningEfforts, []string{"high", "ultra", "low", "none"}) {
		t.Fatalf("authoritative efforts lost: %+v %v", models, err)
	}
}
