package main

import (
	"tosub2-go-port/internal/config"
	"tosub2-go-port/internal/protocol"
)

func savedGrantOptions(cfg config.Config, email, checkpointPath, outputPath string, values map[string]any) protocol.FlowOptions {
	options := protocol.FlowOptions{Email: email, ChatGPTBase: cfg.ChatGPTBase, AuthBase: cfg.AuthBase, ClientID: cfg.CodexClientID, RedirectURI: cfg.CodexRedirectURI, CheckpointPath: checkpointPath, OutputPath: outputPath}
	if cfg.CLI.Enabled() {
		options.OutputMode, _ = values["outputMode"].(string)
		options.SessionOutputPath, _ = values["sessionOutputPath"].(string)
		options.WebOnly, _ = values["webOnly"].(bool)
		options.AccountName, _ = values["accountName"].(string)
		options.Concurrency, _ = values["sub2apiConcurrency"].(int)
		options.Priority, _ = values["priority"].(int)
		options.RateMultiplier, _ = values["rateMultiplier"].(float64)
	}
	return options
}
