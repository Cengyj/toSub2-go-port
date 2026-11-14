//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/transport"
)

func TestRefreshRecoveryRetainsGrantUntilPrivateOutputReadback(t *testing.T) {
	_, source, target := pendingRotationFixture(t)
	pending := refreshRecoveryPath(target)
	data, err := os.ReadFile(pending)
	if err != nil {
		t.Fatal(err)
	}
	var receipt refreshGrantRecovery
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	updated := updateRefreshedDocument(receipt.Source, receipt.Tokens, receipt.ClientID)
	updated["exported_at"] = receipt.AcceptedAt
	if err := protocol.WritePrivateJSON(target, updated); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	calls := 0
	doer := refreshRecoveryDoer(func(context.Context, transport.Request) (*transport.Response, error) {
		calls++
		return nil, errors.New("local completion must not connect")
	})
	if err := refreshExistingExportAt(t.Context(), doer, receipt.AuthBase, "fallback-client", source, target, "first@example.test"); err == nil || !isLocalRefreshFailure(err) {
		t.Fatal("unprotected matching output was treated as committed", err)
	}
	retained, err := os.ReadFile(pending)
	if err != nil || !bytes.Equal(retained, data) || calls != 0 {
		t.Fatal("failed readback removed the receipt, changed tokens or repeated the grant")
	}
	if err := os.Chmod(target, 0600); err != nil {
		t.Fatal(err)
	}
	if err := refreshExistingExportAt(t.Context(), doer, receipt.AuthBase, "fallback-client", source, target, "first@example.test"); err != nil {
		t.Fatal("corrected private output could not complete locally", err)
	}
	if _, err := os.Stat(pending); !errors.Is(err, os.ErrNotExist) || calls != 0 {
		t.Fatal("verified output did not finish with zero network calls")
	}
}
