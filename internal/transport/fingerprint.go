package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/bogdanfinn/tls-client/profiles"
)

type Profile struct {
	Name              string
	UserAgent         string
	AcceptLanguage    string
	Platform          string
	NavigatorPlatform string
	Major             int
	ClientHints       string
}

func normalizeProfile(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "-", "_")
	if strings.HasPrefix(name, "chrome") && !strings.HasPrefix(name, "chrome_") {
		name = strings.Replace(name, "chrome", "chrome_", 1)
	}
	return name
}

// ProfileCandidates lists only Chrome profiles actually bundled in tls-client.
// An unsupported legacy alias resolves to real profiles, never a made-up UA.
func ProfileCandidates(explicit string) []string {
	requested := normalizeProfile(explicit)
	out := []string{}
	requestedMajor := 0
	if _, ok := profiles.MappedTLSClients[requested]; ok && strings.HasPrefix(requested, "chrome_") {
		out = append(out, requested)
		requestedMajor, _ = strconv.Atoi(strings.Split(strings.TrimPrefix(requested, "chrome_"), "_")[0])
	}
	for _, name := range []string{"chrome_133", "chrome_131", "chrome_124", "chrome_120"} {
		major, _ := strconv.Atoi(strings.TrimPrefix(name, "chrome_"))
		if requestedMajor > 0 && major > requestedMajor {
			continue
		}
		if name != requested {
			if _, ok := profiles.MappedTLSClients[name]; ok {
				out = append(out, name)
			}
		}
	}
	return out
}
func ProfileFor(name string) Profile { return ProfileForPlatform(name, runtime.GOOS) }
func ProfileForPlatform(name, platform string) Profile {
	candidates := ProfileCandidates(name)
	if len(candidates) == 0 {
		return Profile{}
	}
	resolved := candidates[0]
	version := strings.Split(strings.TrimPrefix(resolved, "chrome_"), "_")[0]
	major, _ := strconv.Atoi(version)
	label, token, navigator := "Linux", "X11; Linux x86_64", "Linux x86_64"
	switch platform {
	case "darwin", "macOS":
		label, token, navigator = "macOS", "Macintosh; Intel Mac OS X 10_15_7", "MacIntel"
	case "windows", "Windows":
		label, token, navigator = "Windows", "Windows NT 10.0; Win64; x64", "Win32"
	}
	return Profile{Name: resolved, Major: major, Platform: label, NavigatorPlatform: navigator, AcceptLanguage: "zh-CN,zh;q=0.9,en;q=0.8", UserAgent: fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36", token, major), ClientHints: fmt.Sprintf(`"Chromium";v="%d", "Google Chrome";v="%d", "Not.A/Brand";v="99"`, major, major)}
}

// ChooseProfile is kept for source compatibility. A supplied net/http client
// cannot probe TLS profiles and is explicitly rejected; use ProbeFingerprintClient.
func ChooseProfile(ctx context.Context, explicit, validationURL string, client *http.Client) (Profile, error) {
	if client != nil {
		return Profile{}, errors.New("net/http cannot probe TLS profiles; use ProbeFingerprintClient")
	}
	c, e := ProbeFingerprintClient(ctx, explicit, "", validationURL, nil)
	if e != nil {
		return Profile{}, e
	}
	defer c.Close()
	return ProfileFor(c.Profile()), nil
}

const (
	ProfileProbeTimeout      = 15 * time.Second
	ProfileProbeTotalTimeout = 300 * time.Second
	ProfileProbeConcurrency  = 4
)

// FingerprintProbeOptions permits shorter diagnostic/test budgets. Values above
// the production limits are capped; caller cancellation always takes priority.
type FingerprintProbeOptions struct {
	CandidateTimeout time.Duration
	TotalTimeout     time.Duration
	Concurrency      int
}

func (o FingerprintProbeOptions) normalized() FingerprintProbeOptions {
	if o.CandidateTimeout <= 0 || o.CandidateTimeout > ProfileProbeTimeout {
		o.CandidateTimeout = ProfileProbeTimeout
	}
	if o.TotalTimeout <= 0 || o.TotalTimeout > ProfileProbeTotalTimeout {
		o.TotalTimeout = ProfileProbeTotalTimeout
	}
	if o.Concurrency <= 0 || o.Concurrency > ProfileProbeConcurrency {
		o.Concurrency = ProfileProbeConcurrency
	}
	return o
}

// ProbeFingerprintClient probes only fresh TLS identities. Any HTTP response,
// including a security challenge, is a successful TLS connection; the earliest
// preferred responding candidate is retained, never a profile that avoids a
// challenge. No further candidate batch starts after an HTTP response is chosen.
func ProbeFingerprintClient(ctx context.Context, requested, proxyURL, validationURL string, jar *CookieJar) (*FingerprintClient, error) {
	return ProbeFingerprintClientWithOptions(ctx, requested, proxyURL, validationURL, jar, FingerprintProbeOptions{})
}

func ProbeFingerprintClientWithOptions(ctx context.Context, requested, proxyURL, validationURL string, jar *CookieJar, options FingerprintProbeOptions) (*FingerprintClient, error) {
	options = options.normalized()
	// Existing cookies describe a bound session. Do not concurrently send them
	// through several TLS identities or combine unrelated probe responses.
	bound := jar != nil && len(jar.Cookies()) > 0
	if bound {
		options.Concurrency = 1
	}
	c, err := probeFingerprintWithOptions(ctx, requested, validationURL, options, func(name string) (*FingerprintClient, error) {
		candidateJar := NewCookieJar()
		if bound {
			candidateJar = jar
		}
		return newFingerprintClientExact(name, proxyURL, candidateJar)
	})
	if err != nil {
		return nil, err
	}
	if jar != nil {
		// Keep the selected tls-client and its established connections. Only its
		// cookies enter the caller's checkpoint/browser jar; losers never do.
		c.mu.RLock()
		selectedCookies := c.jar.Cookies()
		c.mu.RUnlock()
		jar.Merge(selectedCookies)
		c.SetJar(jar)
	}
	return c, nil
}
func probeFingerprint(ctx context.Context, requested, validationURL string, factory func(string) (*FingerprintClient, error)) (*FingerprintClient, error) {
	return probeFingerprintWithOptions(ctx, requested, validationURL, FingerprintProbeOptions{}, factory)
}

func probeFingerprintWithOptions(parent context.Context, requested, validationURL string, options FingerprintProbeOptions, factory func(string) (*FingerprintClient, error)) (*FingerprintClient, error) {
	options = options.normalized()
	ctx := parent
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, options.TotalTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names := ProfileCandidates(requested)
	if len(names) == 0 {
		return nil, &TransportError{kind: failureProfile}
	}
	if validationURL == "" {
		c, err := factory(names[0])
		if err != nil {
			return nil, classifyTransportError(err, phaseRequest, false)
		}
		return c, nil
	}
	type result struct {
		index  int
		client *FingerprintClient
		err    error
	}
	var failures []*TransportError
	for offset := 0; offset < len(names); offset += options.Concurrency {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := offset + options.Concurrency
		if end > len(names) {
			end = len(names)
		}
		batch := names[offset:end]
		batchCtx, stopBatch := context.WithCancel(ctx)
		results := make(chan result, len(batch))
		for i, name := range batch {
			go func(index int, profile string) {
				probeCtx, stop := context.WithTimeout(batchCtx, options.CandidateTimeout)
				defer stop()
				if err := probeCtx.Err(); err != nil {
					results <- result{index: index, err: err}
					return
				}
				c, err := factory(profile)
				if err == nil && c == nil {
					err = errors.New("TLS probe factory returned no client")
				}
				if err == nil {
					err = c.probeHTTP(probeCtx, Request{Method: http.MethodGet, URL: validationURL, Timeout: options.CandidateTimeout})
				}
				results <- result{index: index, client: c, err: err}
			}(i, name)
		}
		received := make([]bool, len(batch))
		outcomes := make([]result, len(batch))
		selected := -1
		var cancellation error
		done := ctx.Done()
		for count := 0; count < len(batch); {
			select {
			case answer := <-results:
				count++
				received[answer.index], outcomes[answer.index] = true, answer
				if selected < 0 && cancellation == nil {
					for i := range batch {
						if !received[i] {
							break
						}
						if outcomes[i].err == nil {
							selected = i
							stopBatch()
							break
						}
					}
				}
			case <-done:
				cancellation = ctx.Err()
				stopBatch()
				done = nil
			}
		}
		stopBatch()
		if cancellation == nil {
			cancellation = ctx.Err()
		}
		for i, answer := range outcomes {
			if answer.err != nil {
				failures = append(failures, classifyTransportError(answer.err, phaseRequest, false))
			}
			if answer.client != nil && (i != selected || cancellation != nil) {
				answer.client.Close()
			}
		}
		if cancellation != nil {
			return nil, cancellation
		}
		if selected >= 0 {
			return outcomes[selected].client, nil
		}
	}
	return nil, &ProfileProbeError{failures: failures}
}
