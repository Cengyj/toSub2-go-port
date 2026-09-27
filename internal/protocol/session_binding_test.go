package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckpointTransportSessionRoundtripAndMismatch(t *testing.T) {
	c := Checkpoint{Version: 1, Stage: "phone_otp", Email: "fixture@example.com"}
	proxy := "socks5h://user:never-persist-this@host.example:1080"
	BindTransportSession(&c, "chromium-native", "actual-browser-UA", proxy, true)
	data, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(data), "never-persist-this") || strings.Contains(string(data), "host.example") {
		t.Fatal("proxy material leaked into checkpoint")
	}
	var restored Checkpoint
	if e = json.Unmarshal(data, &restored); e != nil {
		t.Fatal(e)
	}
	if e = ValidateTransportSession(restored, "chromium-native", "actual-browser-UA", proxy); e != nil {
		t.Fatal(e)
	}
	for _, candidate := range []struct{ profile, ua, proxy string }{{"chrome_133", "actual-browser-UA", proxy}, {"chromium-native", "changed-browser-UA", proxy}, {"chromium-native", "actual-browser-UA", proxy + "-changed"}} {
		if e = ValidateTransportSession(restored, candidate.profile, candidate.ua, candidate.proxy); e == nil {
			t.Fatal("changed binding accepted")
		} else if strings.Contains(e.Error(), "never-persist-this") {
			t.Fatal("proxy leaked into error")
		}
	}
}
func TestLegacyCheckpointBindingAndMalformedMetadata(t *testing.T) {
	c := Checkpoint{}
	if _, present, e := CheckpointTransportSession(c); e != nil || present {
		t.Fatal("legacy binding incorrectly invented")
	}
	c.Extra = map[string]any{"transport_session": map[string]any{"browser_bound": true}}
	if e := ValidateTransportSession(c, "chromium-native", "ua", ""); e == nil {
		t.Fatal("malformed binding accepted")
	}
}
