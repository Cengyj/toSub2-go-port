package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tlsclient "github.com/bogdanfinn/tls-client"
)

type trackedProbeClient struct {
	tlsclient.HttpClient
	closed atomic.Int32
}

func (c *trackedProbeClient) CloseIdleConnections() {
	c.closed.Add(1)
	c.HttpClient.CloseIdleConnections()
}

func trackedProbeFactory() (func(string) (*FingerprintClient, error), func() map[string]*trackedProbeClient) {
	var mu sync.Mutex
	clients := map[string]*trackedProbeClient{}
	return func(name string) (*FingerprintClient, error) {
			c, err := newFingerprintClientExact(name, "", nil)
			if err != nil {
				return nil, err
			}
			tracked := &trackedProbeClient{HttpClient: c.client}
			c.client = tracked
			mu.Lock()
			clients[name] = tracked
			mu.Unlock()
			return c, nil
		}, func() map[string]*trackedProbeClient {
			mu.Lock()
			defer mu.Unlock()
			out := map[string]*trackedProbeClient{}
			for k, v := range clients {
				out[k] = v
			}
			return out
		}
}

func TestTLSProbeSlowPreferredDoesNotStarveFallback(t *testing.T) {
	firstEntered := make(chan struct{})
	var once sync.Once
	var active, peak atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		if strings.Contains(r.UserAgent(), "Chrome/133.") {
			once.Do(func() { close(firstEntered) })
			<-r.Context().Done()
			return
		}
		if strings.Contains(r.UserAgent(), "Chrome/131.") {
			<-firstEntered
			w.Header().Set("Set-Cookie", "selected=131; Path=/")
			w.WriteHeader(200)
			return
		}
		<-r.Context().Done()
	}))
	defer s.Close()
	factory, snapshot := trackedProbeFactory()
	start := time.Now()
	c, err := probeFingerprintWithOptions(context.Background(), "auto", s.URL, FingerprintProbeOptions{CandidateTimeout: 120 * time.Millisecond, TotalTimeout: time.Second, Concurrency: 4}, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Profile() != "chrome_131" || time.Since(start) > 700*time.Millisecond || peak.Load() > 4 || peak.Load() < 2 {
		t.Fatalf("fallback/profile/budget failed: profile=%s elapsed=%s peak=%d", c.Profile(), time.Since(start), peak.Load())
	}
	if c.jar.HeaderFor(s.URL) != "selected=131" {
		t.Fatal("selected candidate cookie jar lost")
	}
	for name, client := range snapshot() {
		want := int32(1)
		if name == c.Profile() {
			want = 0
		}
		if client.closed.Load() != want {
			t.Fatalf("profile %s close count=%d want=%d", name, client.closed.Load(), want)
		}
	}
}

func TestTLSProbeCancellationAndTotalBudgetCloseEveryClient(t *testing.T) {
	for _, external := range []bool{true, false} {
		name := "total-budget"
		if external {
			name = "parent-cancel"
		}
		t.Run(name, func(t *testing.T) {
			var entered, finished atomic.Int32
			allEntered := make(chan struct{})
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if entered.Add(1) == 4 {
					close(allEntered)
				}
				<-r.Context().Done()
				finished.Add(1)
			}))
			defer s.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			budget := 80 * time.Millisecond
			if external {
				budget = time.Second
				go func() { <-allEntered; cancel() }()
			}
			factory, snapshot := trackedProbeFactory()
			start := time.Now()
			c, err := probeFingerprintWithOptions(ctx, "auto", s.URL, FingerprintProbeOptions{CandidateTimeout: time.Second, TotalTimeout: budget, Concurrency: 4}, factory)
			want := context.DeadlineExceeded
			if external {
				want = context.Canceled
			}
			if c != nil || !errors.Is(err, want) || time.Since(start) > 700*time.Millisecond {
				t.Fatalf("cancellation/budget failed: client=%v err=%v elapsed=%s", c, err, time.Since(start))
			}
			clients := snapshot()
			if len(clients) != 4 {
				t.Fatalf("fixture did not create all candidates: %d", len(clients))
			}
			for name, client := range clients {
				if client.closed.Load() != 1 {
					t.Fatalf("unselected client %s not closed", name)
				}
			}
			deadline := time.Now().Add(time.Second)
			for finished.Load() != entered.Load() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if finished.Load() != entered.Load() {
				t.Fatal("canceled probe connections remained live")
			}
		})
	}
}

func TestTLSProbeCandidatesIsolateAndAdoptOnlySelectedCookies(t *testing.T) {
	var entered atomic.Int32
	all := make(chan struct{})
	var leaked atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			leaked.Store(true)
		}
		if entered.Add(1) == 4 {
			close(all)
		}
		select {
		case <-all:
		case <-r.Context().Done():
			return
		}
		identity := strings.Split(strings.Split(r.UserAgent(), "Chrome/")[1], ".")[0]
		w.Header().Set("Set-Cookie", "profile_"+identity+"=only; Path=/")
		if identity == "133" {
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(403)
		} else {
			w.WriteHeader(200)
		}
	}))
	defer s.Close()
	jar := NewCookieJar()
	c, err := ProbeFingerprintClientWithOptions(context.Background(), "auto", "", s.URL, jar, FingerprintProbeOptions{CandidateTimeout: time.Second, TotalTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Profile() != "chrome_133" || c.jar != jar || leaked.Load() || jar.HeaderFor(s.URL) != "profile_133=only" {
		t.Fatal("challenge changed preferred identity or another candidate contaminated selected cookie jar")
	}
}

func TestTLSProbeSlowChallengeBodyNeverRotatesProfile(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.UserAgent(), "Chrome/133.") {
			w.Header().Set("Cf-Mitigated", "challenge")
			w.Header().Set("Set-Cookie", "probe=challenge; Path=/")
			w.WriteHeader(403)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.WriteHeader(200)
	}))
	defer s.Close()
	jar := NewCookieJar()
	c, err := ProbeFingerprintClientWithOptions(context.Background(), "auto", "", s.URL, jar, FingerprintProbeOptions{CandidateTimeout: 80 * time.Millisecond, TotalTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Profile() != "chrome_133" || jar.HeaderFor(s.URL) != "probe=challenge" {
		t.Fatal("slow challenge body caused identity rotation")
	}
}

func TestTLSProbeExistingJarIsNotSentConcurrentlyAcrossProfiles(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "saved=keep" {
			t.Error("saved jar not reused")
		}
		w.Header().Set("Set-Cookie", "new=keep; Path=/")
		w.WriteHeader(403)
	}))
	defer s.Close()
	jar := NewCookieJar()
	jar.SetFromResponse(s.URL, http.Header{"Set-Cookie": {"saved=keep; Path=/"}})
	c, err := ProbeFingerprintClientWithOptions(context.Background(), "auto", "", s.URL, jar, FingerprintProbeOptions{CandidateTimeout: time.Second, TotalTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if calls.Load() != 1 || c.jar != jar || !strings.Contains(jar.HeaderFor(s.URL), "new=keep") {
		t.Fatal("bound cookie session was probed with concurrent profiles")
	}
}
