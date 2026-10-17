package sub2api

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestUploadSettingsDoNotMutateInputsOrAliasCallerSlices(t *testing.T) {
	input := map[string]any{"baseUrl": "https://sub2.example.test/", "apiKey": "synthetic-key", "groupIds": []int{7, 3, 7}, "modelWhitelist": []string{"model-one", "model-two"}}
	before, _ := json.Marshal(input)
	cfg, opts, err := ParseUploadSettings(input)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIRequestTimezone != DefaultOpenAIRequestTimezone || !reflect.DeepEqual(opts.GroupIDs, []int{7, 3}) || cfg.APIKey != "synthetic-key" {
		t.Fatal("shared settings changed a supported contract")
	}
	opts.GroupIDs[0] = 99
	opts.ModelWhitelist[0] = "changed"
	after, _ := json.Marshal(input)
	if string(before) != string(after) {
		t.Fatal("parsing aliased or mutated caller-owned settings")
	}
}
