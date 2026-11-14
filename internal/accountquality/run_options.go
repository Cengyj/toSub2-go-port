package accountquality

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"tosub2-go-port/internal/modelprobe"
)

// RunOptions selects the model for one explicit observation. It never changes
// the durable upload policy, its revision, or the timing and blocking switches.
type RunOptions struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
}

// DecodeRunOptions distinguishes omission from an explicit null or partial
// object. Both fields must occur exactly once; unknown fields are not ignored.
func DecodeRunOptions(data json.RawMessage) (*RunOptions, error) {
	if len(data) == 0 {
		return nil, nil
	}
	invalid := func() (*RunOptions, error) { return nil, &modelprobe.Error{Code: "invalid_options"} }
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return invalid()
	}
	seen := map[string]bool{}
	var selected RunOptions
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] || key != "model" && key != "reasoningEffort" {
			return invalid()
		}
		seen[key] = true
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return invalid()
		}
		target := &selected.Model
		if key == "reasoningEffort" {
			target = &selected.ReasoningEffort
		}
		if json.Unmarshal(raw, target) != nil {
			return invalid()
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || len(seen) != 2 {
		return invalid()
	}
	if decoder.Decode(new(any)) != io.EOF || validateRunOptions(selected) != nil {
		return invalid()
	}
	return &selected, nil
}

func validateRunOptions(selected RunOptions) error {
	if selected.Model == "" || len(selected.Model) > 128 || selected.Model != strings.TrimSpace(selected.Model) || strings.ContainsAny(selected.Model, "\r\n\t") {
		return &modelprobe.Error{Code: "invalid_options"}
	}
	for _, r := range selected.Model {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:", r)) {
			return &modelprobe.Error{Code: "invalid_options"}
		}
	}
	switch selected.ReasoningEffort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return nil
	default:
		return &modelprobe.Error{Code: "invalid_options"}
	}
}

// ApplyRunOptions returns an execution-only copy. Existing callers can omit
// selected and retain every behavior of the globally configured probe.
func ApplyRunOptions(settings Settings, selected *RunOptions) (Settings, error) {
	if selected == nil {
		return settings, nil
	}
	if err := validateRunOptions(*selected); err != nil {
		return Settings{}, err
	}
	settings.Model, settings.ReasoningEffort = selected.Model, selected.ReasoningEffort
	return settings, nil
}
