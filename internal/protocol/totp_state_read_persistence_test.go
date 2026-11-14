package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"tosub2-go-port/internal/transport"
)

// Status reads cannot depend on the removed Cookie-save barrier. A real state
// change must be recorded by the caller after the returned factor is classified.
func TestResetStatusReadsDoNotSaveUnchangedOperation(t *testing.T) {
	x := newResetFixture(t, false)
	var beforeRead []byte
	awaitingWrite, unchangedWrites, verifiedWrites := false, 0, 0
	journalWriteHook(t, x, func(out map[string]any) error {
		if awaitingWrite {
			awaitingWrite = false
			current, err := json.Marshal(out)
			if err != nil {
				return err
			}
			if bytes.Equal(beforeRead, current) {
				unchangedWrites++
				return errors.New("synthetic obsolete status-save barrier failure")
			}
		}
		if out["activation_confirmation_source"] == "state_read" && verifiedResetActivation(out) {
			verifiedWrites++
		}
		return nil
	})
	x.override = func(step string, _ transport.Request) (*transport.Response, error, bool) {
		switch step {
		case "disable":
			x.remoteFactor = ""
			return nil, io.EOF, true
		case "activate":
			x.remoteFactor = "new-factor"
			return nil, io.EOF, true
		}
		if strings.HasPrefix(step, "info") {
			var err error
			beforeRead, err = json.Marshal(x.flow.resetCapture.out)
			if err != nil {
				t.Fatal(err)
			}
			awaitingWrite = true
			return response(200, resetInfo(x.remoteFactor)), nil, true
		}
		return nil, nil, false
	}
	out, err := x.flow.ResetTOTP(t.Context(), x.path)
	if err != nil || !x.persisted || out["credential_persisted"] != true || unchangedWrites != 0 || verifiedWrites == 0 {
		t.Fatalf("state read depended on unchanged journal write: unchanged=%d confirmed=%d err=%v", unchangedWrites, verifiedWrites, err)
	}
	if out["disable_confirmation_source"] != "state_read" || out["activation_confirmation_source"] != "state_read" {
		t.Fatal("actual confirmation source was not recorded")
	}
	if !reflect.DeepEqual(x.calls, []string{"page", "session", "info1", "disable", "info2", "enroll", "activate", "info3"}) {
		t.Fatal("reconciliation repeated a mutation or added a query", x.calls)
	}
	if _, err := ReadTOTPResetCompletion(x.path, x.flow.Options.Email); err != nil {
		t.Fatal("state-read evidence was not durably completed", err)
	}
}

func TestResetStateReadStillRequiresConfirmationEvidenceSave(t *testing.T) {
	for _, step := range []string{"disable", "activate"} {
		t.Run(step, func(t *testing.T) {
			x := newResetFixture(t, false)
			blocked := errors.New("synthetic confirmation evidence save failure")
			writeAttempts := 0
			journalWriteHook(t, x, func(out map[string]any) error {
				confirmed := out["disable_confirmation_source"] == "state_read"
				if step == "activate" {
					confirmed = out["activation_confirmation_source"] == "state_read"
				}
				if confirmed {
					writeAttempts++
					return blocked
				}
				return nil
			})
			x.override = func(actual string, _ transport.Request) (*transport.Response, error, bool) {
				if actual == step {
					x.remoteFactor = ""
					if step == "activate" {
						x.remoteFactor = "new-factor"
					}
					return nil, io.EOF, true
				}
				return nil, nil, false
			}
			_, err := x.flow.ResetTOTP(t.Context(), x.path)
			if err == nil || !errors.Is(err, blocked) || writeAttempts == 0 || x.persisted {
				t.Fatalf("failed confirmation save allowed credential commit: writes=%d persisted=%t err=%v", writeAttempts, x.persisted, err)
			}
			want := []string{"page", "session", "info1", "disable", "info2"}
			if step == "activate" {
				want = []string{"page", "session", "info1", "disable", "enroll", "activate", "info2"}
			}
			if !reflect.DeepEqual(x.calls, want) {
				t.Fatal("evidence save failure repeated or advanced requests", x.calls)
			}
			if _, err := ReadTOTPResetCompletion(x.path, x.flow.Options.Email); err == nil {
				t.Fatal("unpublished state-read evidence authorized local completion")
			}
			before := len(x.calls)
			if _, err := x.flow.ResetTOTP(t.Context(), x.path); err == nil || len(x.calls) != before {
				t.Fatal("failed evidence save allowed mutation replay")
			}
		})
	}
}
