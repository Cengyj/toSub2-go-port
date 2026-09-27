package challenge

import (
	"context"
	"errors"
	"strings"

	"tosub2-go-port/internal/browser"
)

// ErrVerificationPending preserves a browser session whose provider verification
// has not finished. An automatic retry must not navigate away from that session.
var ErrVerificationPending = errors.New("browser verification pending")

// ErrVerificationRejected identifies a refused document without an active
// provider challenge. Repeatedly navigating it is not a verification strategy.
var ErrVerificationRejected = errors.New("browser verification rejected")

// A resource looked up only by URL can be from an old page or another target.
// Only a main-frame, current-loader diagnostic is evidence about this document.
func (s BrowserSolver) currentDocumentDiagnostic(ctx context.Context) (browser.NetworkDiagnostic, bool) {
	if current, ok := s.Session.(interface {
		CurrentDocumentDiagnostic(context.Context) (browser.NetworkDiagnostic, bool)
	}); ok {
		diagnostic, known := current.CurrentDocumentDiagnostic(ctx)
		return safeVerificationDiagnostic(diagnostic), known
	}
	return browser.NetworkDiagnostic{}, false
}

func verificationContextCause(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return nil
}

func verificationDetail(message string, diagnostic browser.NetworkDiagnostic, documentKnown, pending, domKnown bool) string {
	dom := "unknown"
	if domKnown {
		dom = "clear"
		if pending {
			dom = "pending"
		}
	}
	detail := "document=unknown"
	if documentKnown {
		detail = safeVerificationDiagnostic(diagnostic).Summary()
		if detail == "" {
			detail = "document=observed"
		}
	}
	return message + " (" + detail + ", DOM=" + dom + ")"
}

// Keep error text safe even for alternative BrowserSession implementations.
// Neither page text nor raw JavaScript/CDP errors enter a verification error.
func safeVerificationDiagnostic(diagnostic browser.NetworkDiagnostic) browser.NetworkDiagnostic {
	if diagnostic.Status < 100 || diagnostic.Status > 599 {
		diagnostic.Status = 0
	}
	switch diagnostic.ContentKind {
	case "html", "json", "javascript", "other":
	default:
		diagnostic.ContentKind = ""
	}
	code := func(value string) string {
		if value == "" {
			return ""
		}
		if len(value) > 80 || strings.IndexFunc(value, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == ':')
		}) >= 0 {
			return "unclassified"
		}
		return value
	}
	diagnostic.NetworkError = code(diagnostic.NetworkError)
	diagnostic.BlockedReason = code(diagnostic.BlockedReason)
	diagnostic.CORSError = code(diagnostic.CORSError)
	return diagnostic
}
