package protocol

// These are explicit choices from sub2api's default model/whitelist options,
// kept apart from the account's live manifest. Their presence never confirms
// account permission. Models mentioned by the manifest, even disabled ones,
// are excluded so a suggestion cannot override a real upstream restriction.
func codexCommonModelSuggestions(mentioned map[string]bool) []AccountModel {
	common := []AccountModel{
		{ID: "gpt-6.1-sol", DisplayName: "GPT-6.1 Sol", ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
		{ID: "gpt-6.1-terra", DisplayName: "GPT-6.1 Terra", ReasoningEfforts: []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{ID: "gpt-6-astra", DisplayName: "GPT-6 Astra", ReasoningEfforts: []string{}},
		{ID: "gpt-6-sol", DisplayName: "GPT-6 Sol", ReasoningEfforts: []string{}},
		{ID: "gpt-6-luna", DisplayName: "GPT-6 Luna", ReasoningEfforts: []string{}},
		{ID: "gpt-5.6-sol", DisplayName: "GPT-5.6 Sol", ReasoningEfforts: []string{}},
		{ID: "gpt-5.6-terra", DisplayName: "GPT-5.6 Terra", ReasoningEfforts: []string{}},
		{ID: "gpt-5.6-luna", DisplayName: "GPT-5.6 Luna", ReasoningEfforts: []string{}},
	}
	suggestions := []AccountModel{}
	for _, model := range common {
		if !mentioned[model.ID] {
			suggestions = append(suggestions, model)
		}
	}
	return suggestions
}
