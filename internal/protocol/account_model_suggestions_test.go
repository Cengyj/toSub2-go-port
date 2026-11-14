package protocol

import (
	"context"
	"reflect"
	"testing"

	"tosub2-go-port/internal/transport"
)

func TestAccountModelsCommonChoicesRespectLiveEntriesAndExplicitDenials(t *testing.T) {
	calls := 0
	client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		calls++
		return insightResponse(200, `{"models":[
			{"slug":"gpt-5.6-sol","supported_reasoning_levels":[{"effort":"high"}]},
			{"slug":"gpt-6.1-sol","available":false},
			{"slug":"gpt-6-luna","enabled":false},
			{"slug":"gpt-6-astra","visibility":"hide","model_picker_enabled":false,"reasoning_efforts":["medium"]}
		]}`), nil
	})}
	catalog, err := client.Models(t.Context(), insightTestCredentials())
	if err != nil || calls != 1 || catalog.Source != "codex-models" || len(catalog.Models) != 2 {
		t.Fatal("suggestions changed the real catalog or triggered another request", err)
	}
	if !reflect.DeepEqual(catalog.Models[0].ReasoningEfforts, []string{"high"}) || !reflect.DeepEqual(catalog.Models[1].ReasoningEfforts, []string{"medium"}) {
		t.Fatal("common defaults replaced account-declared model capabilities")
	}
	foundTerra := false
	for _, model := range catalog.SuggestedModels {
		switch model.ID {
		case "gpt-5.6-sol", "gpt-6.1-sol", "gpt-6-luna", "gpt-6-astra":
			t.Fatal("common choices duplicated a live entry or revived an explicit denial")
		case "gpt-6.1-terra":
			foundTerra = true
			if !reflect.DeepEqual(model.ReasoningEfforts, []string{"none", "low", "medium", "high", "xhigh", "max"}) {
				t.Fatal("Terra preset did not retain the reference model's reasoning choices")
			}
		}
	}
	if !foundTerra {
		t.Fatal("missing common model still required typing its ID")
	}
}

func TestAccountModelsCommonChoicesStaySeparateFromEmptyCatalogAndBPSPermissions(t *testing.T) {
	for _, method := range []string{AuthorizationMethodChatGPT, AuthorizationMethodBPS} {
		t.Run(method, func(t *testing.T) {
			creds := insightTestCredentials()
			creds.AuthorizationMethod = method
			client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
				return insightResponse(200, `{"models":[],"allowed":true,"model_catalog":{"models":[]}}`), nil
			})}
			catalog, err := client.Models(t.Context(), creds)
			if err != nil || len(catalog.Models) != 0 || catalog.Message == "" {
				t.Fatal("empty account catalog was replaced with preset permissions", err)
			}
			if method == AuthorizationMethodBPS {
				if len(catalog.SuggestedModels) != 0 {
					t.Fatal("Codex common choices bypassed BPS access discovery")
				}
			} else if len(catalog.SuggestedModels) != 8 || catalog.SuggestedModels[0].ID != "gpt-6.1-sol" {
				t.Fatal("Codex common choices were not returned independently")
			}
		})
	}
	client := &InsightsClient{Transport: insightFixtureDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		return insightResponse(401, ""), nil
	})}
	failed, err := client.Models(t.Context(), insightTestCredentials())
	if err == nil || len(failed.Models) != 0 || len(failed.SuggestedModels) != 0 {
		t.Fatal("failed discovery became an apparently successful preset catalog")
	}
}
