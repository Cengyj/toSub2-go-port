package browser

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// NetworkDiagnostic describes browser resource failures without retaining any
// URL, headers, body, cookies, console output, token or submitted form data.
// Request URLs are matched only by SHA-256. At most 256 resource records plus
// the current main-document record are retained.
type NetworkDiagnostic struct {
	Status        int
	ContentKind   string
	NetworkError  string
	BlockedReason string
	CORSError     string
	Challenge     bool
}

func (d NetworkDiagnostic) Summary() string {
	var fields []string
	if d.Status != 0 {
		fields = append(fields, fmt.Sprintf("HTTP %d", d.Status))
	}
	if d.ContentKind != "" {
		fields = append(fields, "content="+d.ContentKind)
	}
	if d.NetworkError != "" {
		fields = append(fields, d.NetworkError)
	}
	if d.BlockedReason != "" {
		fields = append(fields, "blocked="+d.BlockedReason)
	}
	if d.CORSError != "" {
		fields = append(fields, "cors="+d.CORSError)
	}
	if d.Challenge {
		fields = append(fields, "provider-challenge")
	}
	return strings.Join(fields, ", ")
}

type resourceDiagnostic struct {
	digest         [32]byte
	detail         NetworkDiagnostic
	documentFrame  string
	documentLoader string
	documentDigest [32]byte
	documentDetail NetworkDiagnostic
}

// CurrentDocumentDiagnostic returns evidence for this page's current main
// document only. A same-URL iframe, fetch, old navigation or protocol request
// tab must not establish that the visible challenge document has completed.
// URLs are used transiently and retained only as a digest.
func (s *Session) CurrentDocumentDiagnostic(ctx context.Context) (NetworkDiagnostic, bool) {
	tree, err := s.frameTree(ctx)
	if err != nil {
		return NetworkDiagnostic{}, false
	}
	return s.currentDocumentDiagnostic(tree.Frame.ID, tree.Frame.LoaderID, tree.Frame.URL)
}

func documentURLDigest(raw string) ([32]byte, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return [32]byte{}, false
	}
	u.Fragment, u.RawFragment = "", ""
	return sha256.Sum256([]byte(u.String())), true
}

func (s *Session) currentDocumentDiagnostic(frame, loader, rawURL string) (NetworkDiagnostic, bool) {
	digest, valid := documentURLDigest(rawURL)
	if !valid || frame == "" || loader == "" {
		return NetworkDiagnostic{}, false
	}
	s.diagnosticMu.Lock()
	defer s.diagnosticMu.Unlock()
	if entry := s.diagnosticDocument; entry.documentFrame == frame && entry.documentLoader == loader && entry.documentDigest == digest && entry.documentDetail.Status != 0 {
		return entry.documentDetail, true
	}
	for i := len(s.diagnosticOrder) - 1; i >= 0; i-- {
		entry := s.diagnosticEntries[s.diagnosticOrder[i]]
		if entry.documentFrame == frame && entry.documentLoader == loader && entry.documentDigest == digest && entry.documentDetail.Status != 0 {
			s.diagnosticDocument = entry
			s.diagnosticDocumentRequest = s.diagnosticOrder[i]
			return entry.documentDetail, true
		}
	}
	return NetworkDiagnostic{}, false
}

func (s *Session) ResourceDiagnostic(rawURL string) NetworkDiagnostic {
	digest := sha256.Sum256([]byte(rawURL))
	s.diagnosticMu.Lock()
	defer s.diagnosticMu.Unlock()
	for i := len(s.diagnosticOrder) - 1; i >= 0; i-- {
		entry := s.diagnosticEntries[s.diagnosticOrder[i]]
		if entry.digest == digest {
			return entry.detail
		}
	}
	return NetworkDiagnostic{}
}

func diagnosticCode(raw string) string {
	if len(raw) == 0 || len(raw) > 80 {
		return ""
	}
	for _, r := range raw {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == ':') {
			return ""
		}
	}
	return raw
}

func diagnosticContentKind(raw string) string {
	raw = strings.ToLower(raw)
	switch {
	case strings.Contains(raw, "javascript"), strings.Contains(raw, "ecmascript"):
		return "javascript"
	case strings.Contains(raw, "html"):
		return "html"
	case strings.Contains(raw, "json"):
		return "json"
	case raw == "":
		return ""
	default:
		return "other"
	}
}

func (s *Session) recordResourceDiagnostic(m cdpMessage) {
	if m.Method == "Page.frameNavigated" {
		s.recordMainDocumentNavigation(m)
		return
	}
	if m.Method != "Network.requestWillBeSent" && m.Method != "Network.responseReceived" && m.Method != "Network.responseReceivedExtraInfo" && m.Method != "Network.loadingFailed" {
		return
	}
	var p struct {
		ID       string `json:"requestId"`
		FrameID  string `json:"frameId"`
		LoaderID string `json:"loaderId"`
		Type     string `json:"type"`
		Request  struct {
			URL string `json:"url"`
		} `json:"request"`
		Response struct {
			URL      string                     `json:"url"`
			Status   int                        `json:"status"`
			MIMEType string                     `json:"mimeType"`
			Headers  map[string]json.RawMessage `json:"headers"`
		} `json:"response"`
		ErrorText     string                     `json:"errorText"`
		BlockedReason string                     `json:"blockedReason"`
		StatusCode    int                        `json:"statusCode"`
		Headers       map[string]json.RawMessage `json:"headers"`
		CORS          struct {
			Error string `json:"corsError"`
		} `json:"corsErrorStatus"`
	}
	if json.Unmarshal(m.Params, &p) != nil || p.ID == "" {
		return
	}
	key := m.SessionID + "/" + p.ID
	s.diagnosticMu.Lock()
	defer s.diagnosticMu.Unlock()
	entry, exists := s.diagnosticEntries[key]
	if !exists {
		if s.diagnosticEntries == nil {
			s.diagnosticEntries = map[string]resourceDiagnostic{}
		}
		if len(s.diagnosticOrder) >= 256 {
			delete(s.diagnosticEntries, s.diagnosticOrder[0])
			s.diagnosticOrder = s.diagnosticOrder[1:]
		}
		s.diagnosticOrder = append(s.diagnosticOrder, key)
	}
	switch m.Method {
	case "Network.requestWillBeSent":
		if entry.digest == ([32]byte{}) {
			entry.digest = sha256.Sum256([]byte(p.Request.URL))
		}
	case "Network.responseReceived":
		if entry.digest == ([32]byte{}) {
			entry.digest = sha256.Sum256([]byte(p.Response.URL))
		}
		entry.detail.Status = p.Response.Status
		entry.detail.ContentKind = diagnosticContentKind(p.Response.MIMEType)
		for k, v := range p.Response.Headers {
			if strings.EqualFold(k, "Cf-Mitigated") || strings.EqualFold(k, "X-Cf-Mitigated") {
				var value string
				if json.Unmarshal(v, &value) == nil && strings.EqualFold(value, "challenge") {
					entry.detail.Challenge = true
				}
			}
		}
		// Only responseReceived identifies both the main document and its
		// loader. ExtraInfo can arrive out of order around redirects and must
		// not overwrite this document-bound evidence.
		if m.SessionID == "" && p.Type == "Document" && p.FrameID != "" && p.LoaderID != "" {
			if digest, valid := documentURLDigest(p.Response.URL); valid {
				entry.documentFrame, entry.documentLoader, entry.documentDigest = p.FrameID, p.LoaderID, digest
				entry.documentDetail = NetworkDiagnostic{Status: p.Response.Status, ContentKind: diagnosticContentKind(p.Response.MIMEType)}
				for k, v := range p.Response.Headers {
					if strings.EqualFold(k, "Cf-Mitigated") || strings.EqualFold(k, "X-Cf-Mitigated") {
						var value string
						if json.Unmarshal(v, &value) == nil && strings.EqualFold(value, "challenge") {
							entry.documentDetail.Challenge = true
						}
					}
				}
			}
		}
	case "Network.responseReceivedExtraInfo":
		entry.detail.Status = p.StatusCode
		for k, v := range p.Headers {
			var value string
			if json.Unmarshal(v, &value) != nil {
				continue
			}
			if strings.EqualFold(k, "Content-Type") {
				entry.detail.ContentKind = diagnosticContentKind(value)
			}
			if (strings.EqualFold(k, "Cf-Mitigated") || strings.EqualFold(k, "X-Cf-Mitigated")) && strings.EqualFold(value, "challenge") {
				entry.detail.Challenge = true
			}
		}
	case "Network.loadingFailed":
		entry.detail.NetworkError = diagnosticCode(p.ErrorText)
		entry.detail.BlockedReason = diagnosticCode(p.BlockedReason)
		entry.detail.CORSError = diagnosticCode(p.CORS.Error)
		if entry.documentFrame != "" {
			entry.documentDetail.NetworkError = entry.detail.NetworkError
			entry.documentDetail.BlockedReason = entry.detail.BlockedReason
			entry.documentDetail.CORSError = entry.detail.CORSError
		}
		if key == s.diagnosticDocumentRequest {
			s.diagnosticDocument.documentDetail.NetworkError = entry.detail.NetworkError
			s.diagnosticDocument.documentDetail.BlockedReason = entry.detail.BlockedReason
			s.diagnosticDocument.documentDetail.CORSError = entry.detail.CORSError
		}
	}
	s.diagnosticEntries[key] = entry
	if entry.documentFrame != "" && entry.documentFrame == s.diagnosticDocument.documentFrame && entry.documentLoader == s.diagnosticDocument.documentLoader && entry.documentDigest == s.diagnosticDocument.documentDigest {
		s.diagnosticDocument = entry
		s.diagnosticDocumentRequest = key
	}
}

// Page.frameNavigated identifies the main frame before its scripts can evict
// the HTTP response from the bounded resource cache. CDP response and frame
// events can arrive in either order; both paths populate this one pinned record.
func (s *Session) recordMainDocumentNavigation(m cdpMessage) {
	if m.SessionID != "" {
		return
	}
	var p struct {
		Frame struct {
			ID       string `json:"id"`
			ParentID string `json:"parentId"`
			LoaderID string `json:"loaderId"`
			URL      string `json:"url"`
		} `json:"frame"`
	}
	if json.Unmarshal(m.Params, &p) != nil || p.Frame.ID == "" || p.Frame.ParentID != "" {
		return
	}
	digest, _ := documentURLDigest(p.Frame.URL)
	s.diagnosticMu.Lock()
	defer s.diagnosticMu.Unlock()
	if entry := s.diagnosticDocument; entry.documentFrame == p.Frame.ID && entry.documentLoader == p.Frame.LoaderID && entry.documentDigest == digest {
		return
	}
	s.diagnosticDocument = resourceDiagnostic{documentFrame: p.Frame.ID, documentLoader: p.Frame.LoaderID, documentDigest: digest}
	s.diagnosticDocumentRequest = ""
	for i := len(s.diagnosticOrder) - 1; i >= 0; i-- {
		entry := s.diagnosticEntries[s.diagnosticOrder[i]]
		if entry.documentFrame == p.Frame.ID && entry.documentLoader == p.Frame.LoaderID && entry.documentDigest == digest {
			s.diagnosticDocument = entry
			s.diagnosticDocumentRequest = s.diagnosticOrder[i]
			break
		}
	}
}
