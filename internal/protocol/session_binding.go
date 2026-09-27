package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// TransportSession is non-secret identity metadata attached to private legacy
// checkpoints. Proxy credentials remain in the OS credential store; only the
// digest of the complete effective proxy URL is stored here.
type TransportSession struct {
	BrowserBound bool   `json:"browser_bound"`
	ProxySHA256  string `json:"proxy_sha256"`
	UserAgent    string `json:"user_agent"`
	TLSProfile   string `json:"tls_profile"`
}

func ProxyDigest(proxy string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(proxy)))
	return hex.EncodeToString(sum[:])
}
func BindTransportSession(c *Checkpoint, profile, ua, proxy string, browserBound bool) {
	if c == nil {
		return
	}
	if c.Extra == nil {
		c.Extra = map[string]any{}
	}
	c.Extra["transport_session"] = TransportSession{BrowserBound: browserBound, ProxySHA256: ProxyDigest(proxy), UserAgent: ua, TLSProfile: profile}
}

// CheckpointTransportSession reports whether a checkpoint carries identity
// metadata. Legacy checkpoints have no binding record; callers must decide
// whether to restart login before using their cookies in a new transport.
func CheckpointTransportSession(c Checkpoint) (TransportSession, bool, error) {
	raw, ok := c.Extra["transport_session"]
	if !ok {
		return TransportSession{}, false, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return TransportSession{}, true, errors.New("CHECKPOINT_SESSION_INVALID: identity metadata cannot be read")
	}
	var binding TransportSession
	if json.Unmarshal(data, &binding) != nil || binding.ProxySHA256 == "" || binding.UserAgent == "" || binding.TLSProfile == "" {
		return TransportSession{}, true, errors.New("CHECKPOINT_SESSION_INVALID: incomplete identity metadata")
	}
	digest, err := hex.DecodeString(binding.ProxySHA256)
	if err != nil || len(digest) != 32 {
		return TransportSession{}, true, errors.New("CHECKPOINT_SESSION_INVALID: invalid proxy digest")
	}
	return binding, true, nil
}
func ValidateTransportSession(c Checkpoint, profile, ua, proxy string) error {
	binding, present, err := CheckpointTransportSession(c)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if binding.ProxySHA256 != ProxyDigest(proxy) {
		return errors.New("CHECKPOINT_SESSION_MISMATCH: proxy session changed; re-login is required")
	}
	if binding.UserAgent != ua {
		return errors.New("CHECKPOINT_SESSION_MISMATCH: User-Agent changed; re-login is required")
	}
	if binding.TLSProfile != profile || (binding.BrowserBound && profile != "chromium-native") {
		return errors.New("CHECKPOINT_SESSION_MISMATCH: TLS or browser binding changed; re-login is required")
	}
	return nil
}
