package transport

import (
	"sort"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	utls "github.com/bogdanfinn/utls"
)

// NewCurlCompatibleFingerprintClientExact retains the selected profile while
// matching the original curl_cffi 0.15.0 Chrome146 wire identity. Compatibility
// applies only to chrome_146; other explicit profiles use their bundled identity.
// Navigation defaults belong to the caller, not to the transport.
func NewCurlCompatibleFingerprintClientExact(profile, proxy string, jar *CookieJar, extraOptions ...tlsclient.HttpClientOption) (*FingerprintClient, error) {
	return newFingerprintClientWithMode(normalizeProfile(profile), proxy, jar, true, extraOptions...)
}

func curlCompatibleChrome146(base profiles.ClientProfile) profiles.ClientProfile {
	hello := base.GetClientHelloId()
	factory := hello.SpecFactory
	hello.SpecFactory = func() (utls.ClientHelloSpec, error) {
		spec, err := factory()
		if err != nil {
			return spec, err
		}
		// curl_cffi's Chrome146 omits trust_anchors. Derive a fresh spec so
		// the shared bundled profile, including ordinary clients, stays intact.
		extensions := spec.Extensions[:0]
		for _, extension := range spec.Extensions {
			if generic, ok := extension.(*utls.GenericExtension); ok && generic.Id == 0xca34 {
				continue
			}
			extensions = append(extensions, extension)
		}
		spec.Extensions = extensions
		return spec, nil
	}
	return profiles.NewClientProfile(hello, base.GetSettings(), base.GetSettingsOrder(), base.GetPseudoHeaderOrder(), base.GetConnectionFlow(), base.GetPriorities(), base.GetHeaderPriority(), base.GetStreamID(), base.GetAllowHTTP(), base.GetHttp3Settings(), base.GetHttp3SettingsOrder(), base.GetHttp3PriorityParam(), base.GetHttp3PseudoHeaderOrder(), base.GetHttp3SendGreaseFrames())
}

func applyCurlCompatibleHeaders(headers fhttp.Header) {
	if _, supplied := headers["Accept-Encoding"]; !supplied {
		headers.Set("Accept-Encoding", "gzip, deflate, br")
	}
	// curl places its default Chrome headers first, then caller headers in
	// their supplied order. Request uses a map, so keep additional headers
	// deterministic and put the generated body length last, as curl does.
	order := []string{"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "upgrade-insecure-requests", "user-agent", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest", "accept-encoding", "accept-language", "priority"}
	known := make(map[string]bool, len(order)+1)
	for _, name := range order {
		known[name] = true
	}
	known["content-length"] = true
	var additional []string
	for name := range headers {
		name = strings.ToLower(name)
		if !known[name] && name != strings.ToLower(fhttp.HeaderOrderKey) && name != strings.ToLower(fhttp.PHeaderOrderKey) {
			additional = append(additional, name)
		}
	}
	sort.Strings(additional)
	order = append(order, additional...)
	order = append(order, "content-length")
	headers[fhttp.HeaderOrderKey] = order
}
