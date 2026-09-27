package browser

import (
	"context"
	"encoding/json"
	"errors"
)

func (s *Session) BoundDeviceID() string {
	s.bindingMu.Lock()
	defer s.bindingMu.Unlock()
	return s.DeviceID
}

// UpdateDeviceID allows a browser opened for an early Cloudflare challenge to
// receive the login device identifier once the protocol has resolved it. An
// established, different device cannot be silently reused in the same session.
func (s *Session) UpdateDeviceID(ctx context.Context, device string) error {
	if device == "" {
		return nil
	}
	s.bindingMu.Lock()
	defer s.bindingMu.Unlock()
	if s.DeviceID != "" && s.DeviceID != device {
		return errors.New("BROWSER_DEVICE_MISMATCH: a new device requires a fresh browser session")
	}
	if s.deviceScriptID == "" {
		source := "(() => { try { localStorage.setItem('oai-did', " + jsString(device) + "); } catch (_) {} })();"
		raw, err := s.call(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": source})
		if err != nil {
			return err
		}
		var result struct {
			ID string `json:"identifier"`
		}
		if err = json.Unmarshal(raw, &result); err != nil {
			return err
		}
		s.deviceScriptID = result.ID
	}
	// The current page may already be loaded when the identifier becomes known.
	if _, err := s.Evaluate(ctx, "(() => { try { localStorage.setItem('oai-did', "+jsString(device)+"); return true; } catch (_) { return false; } })()"); err != nil {
		return err
	}
	cookies, err := s.CookiesContext(ctx)
	if err != nil {
		return err
	}
	var updates []map[string]any
	for _, cookie := range cookies {
		if cookie["name"] == "oai-did" {
			cookie["value"] = device
			updates = append(updates, cookie)
		}
	}
	if len(updates) > 0 {
		if err = s.SetCookiesContext(ctx, updates); err != nil {
			return err
		}
	}
	s.DeviceID = device
	return nil
}
