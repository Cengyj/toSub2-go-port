package sub2api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestMutationRedirectIsUnknownAndNeverFollowed(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/redirect-after-commit")
				w.WriteHeader(status)
			}))
			defer server.Close()
			_, err := New(Config{BaseURL: server.URL, APIKey: "fixture-key"}).Update(t.Context(), "12", map[string]any{"status": "active"})
			if !IsMutationOutcomeUnknown(err) || !errors.Is(err, ErrMutationOutcomeUnknown) || !IsRemoteStatus(err, status) || calls.Load() != 1 {
				t.Fatalf("redirect lost uncertainty or replayed mutation: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}
