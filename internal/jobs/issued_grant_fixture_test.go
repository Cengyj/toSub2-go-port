package jobs

import (
	"encoding/base64"
	"tosub2-go-port/internal/protocol"
)

func issuedGrantFixture(email, method string) protocol.SavedOAuthGrant {
	if method == "" {
		method = "chatgpt"
	}
	client, redirect := protocol.DefaultCodexClientID, protocol.DefaultCodexRedirectURI
	if method == "bps" {
		client, redirect = protocol.DefaultBPSClientID, protocol.DefaultBPSRedirectURI
	}
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"`+email+`","sid":"synthetic-account"}`)) + ".fixture"
	return protocol.SavedOAuthGrant{Version: 1, Email: email, AuthorizationMethod: method, AuthBase: "https://auth.openai.com", ChatGPTBase: "https://chatgpt.com", ClientID: client, RedirectURI: redirect, Tokens: protocol.TokenSet{AccessToken: token, RefreshToken: "synthetic-refresh", IDToken: token}}
}
