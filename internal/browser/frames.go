package browser

import (
	"context"
	"errors"
	"sort"
	"time"
)

// configureFrameTarget registers descendants and proxy authentication in each
// cross-site renderer. These sessions share the browser profile and transport;
// no credentials, challenge tokens or DOM state leave this browser.
func (s *Session) configureFrameTarget(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, method := range []string{"Page.enable", "Runtime.enable"} {
		if _, err := s.callTarget(ctx, sessionID, method, nil); err != nil {
			return
		}
	}
	_, _ = s.callTarget(ctx, sessionID, "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": false, "flatten": true})
	if s.proxyUser != "" {
		_, _ = s.callTarget(ctx, sessionID, "Fetch.enable", map[string]any{"handleAuthRequests": true})
	}
}

func (s *Session) frameTargetIDs() []string {
	s.targetMu.Lock()
	defer s.targetMu.Unlock()
	ids := make([]string, 0, len(s.targetSessions))
	for id := range s.targetSessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

var ErrInteractiveBrowserRequired = errors.New("BROWSER_INTERACTION_REQUIRED: this Chromium session was started headless and cannot become visible; restart the service with TOSUB2_BROWSER_HEADLESS=0 in an accessible graphical desktop and retry with a new browser session. A container also requires a desktop/display service; setting the variable alone is insufficient. On macOS, use the native service in the logged-in desktop session")

// Present exposes the existing page for the account owner to complete a
// provider's interactive verification. It never clicks a challenge or changes
// the browser identity. Headless Chromium cannot become a visible window;
// returning an explicit error prevents reporting an inaccessible prompt.
func (s *Session) Present(ctx context.Context) error {
	if s.headless {
		return ErrInteractiveBrowserRequired
	}
	_, err := s.call(ctx, "Page.bringToFront", nil)
	return err
}
