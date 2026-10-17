package sub2api

import (
	"errors"

	"tosub2-go-port/internal/transport"
)

var (
	// ErrMutationRejected is an explicit business refusal, even when HTTP was
	// successful. Remote response text is deliberately excluded.
	ErrMutationRejected = errors.New("Sub2API rejected the requested change")
	// ErrMutationOutcomeUnknown means the response cannot confirm what changed.
	// It never authorizes replaying a mutation. Reconcile remote state first.
	ErrMutationOutcomeUnknown = errors.New("Sub2API mutation result is unknown; verify remote state before retrying")
)

// IsMutationOutcomeUnknown is shared by manual uploads and workflow callers.
// An HTTP 5xx or lost response can follow a committed mutation; an explicit
// refusal and proven before-send failure cannot. Unknown errors stay cautious.
func IsMutationOutcomeUnknown(err error) bool {
	if err == nil || err == ErrMutationRejected {
		return false
	}
	switch failure := err.(type) {
	case *uploadPreparationError:
		return false
	case *uploadRecoveryError:
		return failure.started && IsMutationOutcomeUnknown(failure.cause)
	case *RemoteError:
		// A redirect can follow a committed POST. Refusing to follow it
		// prevents replay, but does not prove that the write was rejected.
		return failure.StatusCode < 400 || failure.StatusCode >= 500
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return true
		}
		for _, child := range children {
			if IsMutationOutcomeUnknown(child) {
				return true
			}
		}
		return false
	}
	if transport.IsRequestDefinitelyUnsent(err) {
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if cause := wrapped.Unwrap(); cause != nil {
			return IsMutationOutcomeUnknown(cause)
		}
	}
	return true
}

// A failed lookup or local validation never started a write, even when its
// underlying HTTP failure would be uncertain in the context of a mutation.
type uploadPreparationError struct{ cause error }

func (e *uploadPreparationError) Error() string { return e.cause.Error() }
func (e *uploadPreparationError) Unwrap() error { return e.cause }

func classifyMutationError(err error) error {
	if err != nil && !errors.Is(err, ErrMutationOutcomeUnknown) && IsMutationOutcomeUnknown(err) {
		// Keep the HTTP/transport cause for diagnostics and errors.Is/As while
		// making uncertainty visible to callers that use only SafeError.
		return errors.Join(ErrMutationOutcomeUnknown, err)
	}
	return err
}

// mutationRejection traverses only protocol envelopes, never account fields or
// arbitrary nested metadata. An outer success cannot hide a nested refusal.
func mutationRejection(value any) error {
	for depth := 0; depth < 64; depth++ {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		if object["ok"] == false || object["success"] == false {
			return ErrMutationRejected
		}
		nested, exists := object["data"]
		if !exists {
			return nil
		}
		value = nested
	}
	return ErrMutationOutcomeUnknown
}

type uploadCreationOutcome struct {
	created, failed, unknown int
	ids                      []any
}

// uploadCreationResult accepts the deployed boolean and numeric response
// shapes, but requires positive evidence instead of assuming every requested
// account was created. Conflicting or malformed counts invalidate the result.
func uploadCreationResult(value any, requested int) (uploadCreationOutcome, error) {
	unknown := uploadCreationOutcome{unknown: requested}
	rejection := mutationRejection(value)
	if rejection != nil && !errors.Is(rejection, ErrMutationRejected) {
		return unknown, rejection
	}
	created, failed := -1, -1
	confirmed := false
	var ids []any
	for depth := 0; depth < 64; depth++ {
		object, ok := value.(map[string]any)
		if !ok {
			break
		}
		confirmed = confirmed || object["ok"] == true || object["success"] == true
		for _, fields := range []struct {
			keys []string
			dst  *int
		}{
			{[]string{"created", "success_count", "success", "imported", "count"}, &created},
			{[]string{"failed", "failure_count"}, &failed},
		} {
			for _, key := range fields.keys {
				raw, exists := object[key]
				if !exists {
					continue
				}
				if key == "success" {
					if _, boolean := raw.(bool); boolean {
						continue
					}
				}
				count, valid := nonnegativeInteger(raw)
				if !valid || count > int64(requested) || (*fields.dst >= 0 && *fields.dst != int(count)) {
					return unknown, ErrMutationOutcomeUnknown
				}
				*fields.dst = int(count)
			}
		}
		for _, key := range []string{"account_ids", "ids"} {
			if raw, exists := object[key]; exists && raw != nil {
				list, ok := raw.([]any)
				if !ok {
					return unknown, ErrMutationOutcomeUnknown
				}
				seen := map[int64]bool{}
				current := []any{}
				for _, raw := range list {
					id := intValue(raw)
					if id <= 0 || seen[id] {
						return unknown, ErrMutationOutcomeUnknown
					}
					seen[id] = true
					current = append(current, id)
				}
				if len(current) > requested {
					return unknown, ErrMutationOutcomeUnknown
				}
				if ids == nil {
					ids = current
				} else {
					// Aliases and nested envelopes must describe the same created
					// records. A conflicting identity is not evidence of success.
					if len(ids) != len(current) {
						return unknown, ErrMutationOutcomeUnknown
					}
					for _, prior := range ids {
						if !seen[prior.(int64)] {
							return unknown, ErrMutationOutcomeUnknown
						}
					}
				}
			}
		}
		nested, exists := object["data"]
		if !exists {
			break
		}
		value = nested
	}
	if created < 0 {
		if rejection != nil {
			if failed < 0 {
				failed = requested
			}
			created = 0
		} else {
			if !confirmed {
				return unknown, ErrMutationOutcomeUnknown
			}
			created = requested
			if failed > 0 {
				created -= failed
			}
		}
	}
	if failed < 0 {
		failed = 0
	}
	if created+failed > requested || len(ids) > created {
		return unknown, ErrMutationOutcomeUnknown
	}
	if rejection != nil && created == requested {
		return unknown, ErrMutationOutcomeUnknown
	}
	result := uploadCreationOutcome{created: created, failed: failed, unknown: requested - created - failed, ids: ids}
	if result.unknown > 0 {
		return result, ErrMutationOutcomeUnknown
	}
	if failed > 0 {
		return result, ErrMutationRejected
	}
	return result, nil
}
