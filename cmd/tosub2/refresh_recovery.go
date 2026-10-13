package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/protocol"
)

// This file never shares a name with an unrelated browser login checkpoint.
const refreshRecoverySuffix = ".refresh-grant-recovery.json"

var errRefreshLocal = errors.New("local refresh recovery failure")

// refreshLocalError separates local persistence/identity failures from an
// authentication rejection. Reauthorization must not react by logging in again.
type refreshLocalError struct {
	Code string
	Err  error
}

func (e *refreshLocalError) Error() string        { return e.Code + ": " + e.Err.Error() }
func (e *refreshLocalError) Unwrap() error        { return e.Err }
func (e *refreshLocalError) Is(target error) bool { return target == errRefreshLocal }
func isLocalRefreshFailure(err error) bool        { return errors.Is(err, errRefreshLocal) }

func refreshLocal(code, message string, cause error) error {
	if cause == nil {
		cause = errors.New(message)
	} else {
		cause = fmt.Errorf("%s: %w", message, cause)
	}
	return &refreshLocalError{Code: code, Err: cause}
}

type refreshGrantRecovery struct {
	Version      int               `json:"version"`
	Type         string            `json:"type"`
	AcceptedAt   string            `json:"accepted_at"`
	AuthBase     string            `json:"auth_base"`
	ClientID     string            `json:"client_id"`
	SourcePath   string            `json:"source_path"`
	TargetPath   string            `json:"target_path"`
	TargetSHA256 string            `json:"target_sha256,omitempty"`
	TargetAbsent bool              `json:"target_absent"`
	SourceSHA256 string            `json:"source_sha256"`
	Source       map[string]any    `json:"source"`
	Tokens       protocol.TokenSet `json:"tokens"`
}

func refreshRecoveryPath(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+refreshRecoverySuffix)
}

// decodeRefreshDocument retains numeric values exactly, including unknown
// metadata and all accounts after the first, which the legacy CLI preserved.
func decodeRefreshDocument(data []byte) (map[string]any, error) {
	var raw map[string]any
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if d.Decode(&raw) != nil {
		return nil, errors.New("refresh source is not a JSON object")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, errors.New("refresh source contains trailing data")
	}
	return raw, nil
}

func refreshDocumentHash(raw map[string]any) (string, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}

func refreshSourceIdentity(raw map[string]any, fallbackClient string) (clientID, email, refreshToken string, err error) {
	invalid := errors.New("refresh source has an invalid type, version, first account or credentials")
	if raw["type"] != "sub2api-data" {
		return "", "", "", invalid
	}
	// Legacy JS accepts a missing version and upgrades it to v1. Explicit
	// unknown versions are rejected before a rotating grant can be consumed.
	if version, present := raw["version"]; present {
		b, e := json.Marshal(version)
		var number float64
		if e != nil || json.Unmarshal(b, &number) != nil || number != 1 {
			return "", "", "", invalid
		}
	}
	accounts, ok := raw["accounts"].([]any)
	if !ok || len(accounts) == 0 {
		return "", "", "", invalid
	}
	account, ok := accounts[0].(map[string]any)
	if !ok {
		return "", "", "", invalid
	}
	creds, ok := account["credentials"].(map[string]any)
	if !ok {
		return "", "", "", invalid
	}
	refreshToken, _ = creds["refresh_token"].(string)
	if refreshToken == "" {
		return "", "", "", invalid
	}
	extra, _ := account["extra"].(map[string]any)
	clientID, _ = creds["client_id"].(string)
	extraClient, _ := extra["client_id"].(string)
	if clientID != "" && extraClient != "" && clientID != extraClient {
		return "", "", "", errors.New("refresh first account has conflicting OAuth client identities")
	}
	if clientID == "" {
		clientID = extraClient
	}
	if clientID == "" {
		clientID = fallbackClient
	}
	if clientID == "" {
		return "", "", "", invalid
	}
	credentialEmail, _ := creds["email"].(string)
	extraEmail, _ := extra["email"].(string)
	credentialEmail, extraEmail = strings.TrimSpace(credentialEmail), strings.TrimSpace(extraEmail)
	if credentialEmail != "" && extraEmail != "" && !strings.EqualFold(credentialEmail, extraEmail) {
		return "", "", "", errors.New("refresh first account has conflicting email identities")
	}
	email = credentialEmail
	if email == "" {
		email = extraEmail
	}
	if email == "" {
		if idToken, _ := creds["id_token"].(string); idToken != "" {
			if claims, e := protocol.DecodeJWTClaims(idToken); e == nil {
				email, _ = claims["email"].(string)
			}
		}
	}
	return clientID, strings.ToLower(strings.TrimSpace(email)), refreshToken, nil
}

func refreshExistingFile(ctx context.Context, doer protocol.Doer, authBase, clientID, path, expectedEmail string) error {
	return refreshExistingExportAt(ctx, doer, authBase, clientID, path, path, expectedEmail)
}

// refreshExistingExportAt keeps an explicitly selected CLI source untouched
// when its destination differs. Recovery is attached to the stable destination,
// never the CLI's random temporary job directory.
func refreshExistingExportAt(ctx context.Context, doer protocol.Doer, authBase, clientID, sourcePath, targetPath, expectedEmail string) error {
	if handled, err := finishPendingRefreshAt(ctx, authBase, clientID, sourcePath, targetPath, expectedEmail); handled || err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := os.ReadFile(sourcePath)
	if err != nil {
		return refreshLocal("REFRESH_SOURCE_INVALID", "cannot read refresh source; authentication was not started", err)
	}
	raw, err := decodeRefreshDocument(b)
	if err != nil {
		return refreshLocal("REFRESH_SOURCE_INVALID", "cannot parse refresh source; authentication was not started", err)
	}
	effectiveClient, sourceEmail, oldRefresh, err := refreshSourceIdentity(raw, clientID)
	if err != nil {
		return refreshLocal("REFRESH_SOURCE_INVALID", "cannot validate refresh source; authentication was not started", err)
	}
	if expectedEmail != "" && !strings.EqualFold(strings.TrimSpace(expectedEmail), sourceEmail) {
		return refreshLocal("REFRESH_SOURCE_INVALID", "refresh source identity differs from the requested account", nil)
	}
	absolute, err := filepath.Abs(targetPath)
	if err != nil {
		return refreshLocal("REFRESH_SOURCE_INVALID", "refresh target path is invalid", err)
	}
	digest, err := refreshDocumentHash(raw)
	if err != nil {
		return refreshLocal("REFRESH_SOURCE_INVALID", "refresh source cannot be preserved", err)
	}
	absoluteSource, err := filepath.Abs(sourcePath)
	if err != nil {
		return refreshLocal("REFRESH_SOURCE_INVALID", "refresh source path is invalid", err)
	}
	before, err := os.ReadFile(targetPath)
	targetAbsent := errors.Is(err, os.ErrNotExist)
	if err != nil && !targetAbsent {
		return refreshLocal("REFRESH_SOURCE_INVALID", "refresh destination cannot be prepared; authentication was not started", err)
	}
	targetDigest := ""
	if !targetAbsent {
		targetDigest = refreshBytesHash(before)
	}
	tokens, err := protocol.RefreshOAuthToken(ctx, doer, authBase, effectiveClient, oldRefresh)
	if err != nil {
		return err
	}
	// Do not consult ctx before this durable write: cancellation may race
	// with a successful remote rotation of a one-use refresh token.
	recovery := refreshGrantRecovery{Version: 1, Type: "tosub2-refresh-grant", AcceptedAt: time.Now().UTC().Format(time.RFC3339Nano),
		AuthBase: strings.TrimRight(authBase, "/"), ClientID: effectiveClient, SourcePath: absoluteSource, TargetPath: absolute,
		TargetSHA256: targetDigest, TargetAbsent: targetAbsent, SourceSHA256: digest, Source: raw, Tokens: tokens}
	if err := protocol.WritePrivateJSON(refreshRecoveryPath(targetPath), recovery); err != nil {
		return refreshLocal("REFRESH_GRANT_UNSAVED", "server accepted refresh but its new tokens could not be saved; do not automatically retry or log in", err)
	}
	_, err = finishPendingRefreshAt(ctx, authBase, clientID, sourcePath, targetPath, expectedEmail)
	return err
}

// finishPendingRefresh is a strictly local runner entrypoint. It must be called
// before credential/proxy/TLS/browser initialization. Its handled return stays
// true for invalid recovery files so callers cannot start fresh authentication.
func finishPendingRefresh(ctx context.Context, authBase, clientID, path, expectedEmail string) (bool, error) {
	return finishPendingRefreshAt(ctx, authBase, clientID, path, path, expectedEmail)
}

func refreshBytesHash(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func finishPendingRefreshAt(ctx context.Context, authBase, clientID, sourcePath, targetPath, expectedEmail string) (bool, error) {
	pendingPath := refreshRecoveryPath(targetPath)
	f, err := os.Open(pendingPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "cannot read private refresh recovery", err)
	}
	defer f.Close()
	if err := privatefile.Verify(f); err != nil {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "refresh recovery permissions are not private", err)
	}
	var recovery refreshGrantRecovery
	d := json.NewDecoder(f)
	d.UseNumber()
	if d.Decode(&recovery) != nil {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "private refresh recovery is malformed", nil)
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "private refresh recovery contains trailing data", nil)
	}
	// Close the handle before removing the file, including on Windows.
	if err := f.Close(); err != nil {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "cannot close private refresh recovery", err)
	}
	absolute, err := filepath.Abs(targetPath)
	if err != nil {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "refresh target path is invalid", err)
	}
	absoluteSource, err := filepath.Abs(sourcePath)
	if err != nil {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "refresh source path is invalid", err)
	}
	digest, err := refreshDocumentHash(recovery.Source)
	if err != nil || recovery.Version != 1 || recovery.Type != "tosub2-refresh-grant" || recovery.TargetPath != absolute || recovery.SourcePath != absoluteSource ||
		recovery.AuthBase != strings.TrimRight(authBase, "/") || recovery.SourceSHA256 == "" || recovery.SourceSHA256 != digest || recovery.Tokens.AccessToken == "" {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "private refresh recovery does not match its source or destination", nil)
	}
	if recovery.TargetAbsent == (recovery.TargetSHA256 != "") {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "private refresh recovery has an invalid destination snapshot", nil)
	}
	if _, err := time.Parse(time.RFC3339Nano, recovery.AcceptedAt); err != nil {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "private refresh recovery has an invalid acceptance time", nil)
	}
	effectiveClient, sourceEmail, _, err := refreshSourceIdentity(recovery.Source, clientID)
	if err != nil || effectiveClient != recovery.ClientID || (expectedEmail != "" && !strings.EqualFold(strings.TrimSpace(expectedEmail), sourceEmail)) {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "private refresh recovery has a different account or OAuth client", nil)
	}
	// Work on a copy: retaining the original source is essential for safely
	// retrying a write while preserving untouched accounts and custom fields.
	encoded, err := json.Marshal(recovery.Source)
	if err != nil {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "private refresh source cannot be restored", err)
	}
	updated, err := decodeRefreshDocument(encoded)
	if err != nil {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "private refresh source cannot be restored", err)
	}
	updated = updateRefreshedDocument(updated, recovery.Tokens, effectiveClient)
	updated["exported_at"] = recovery.AcceptedAt
	_, resultEmail, _, err := refreshSourceIdentity(updated, clientID)
	if err != nil || (sourceEmail != "" && !strings.EqualFold(sourceEmail, resultEmail)) {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "accepted refresh token identity differs from the original account; grant retained privately", nil)
	}
	resultDigest, err := refreshDocumentHash(updated)
	if err != nil {
		return true, refreshLocal("REFRESH_OUTPUT_PENDING", "accepted grant is saved but output could not be prepared", err)
	}
	currentBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		return true, refreshLocal("REFRESH_OUTPUT_PENDING", "accepted grant is saved; restore access to the original output and retry locally", err)
	}
	current, err := decodeRefreshDocument(currentBytes)
	if err != nil {
		return true, refreshLocal("REFRESH_OUTPUT_PENDING", "accepted grant is saved but current output no longer matches its source", err)
	}
	currentDigest, err := refreshDocumentHash(current)
	if err != nil || (currentDigest != recovery.SourceSHA256 && !(absoluteSource == absolute && currentDigest == resultDigest)) {
		return true, refreshLocal("REFRESH_RECOVERY_INVALID", "current output was changed since refresh; refusing to overwrite it or repeat the grant", nil)
	}
	if err := ctx.Err(); err != nil {
		return true, refreshLocal("REFRESH_OUTPUT_PENDING", "accepted grant is saved for a local retry after cancellation", err)
	}
	targetBytes, readErr := os.ReadFile(targetPath)
	missing := errors.Is(readErr, os.ErrNotExist)
	if readErr != nil && !missing {
		return true, refreshLocal("REFRESH_OUTPUT_PENDING", "accepted grant is saved; restore access to the destination and retry locally", readErr)
	}
	targetIsResult := false
	if readErr == nil {
		if target, err := decodeRefreshDocument(targetBytes); err == nil {
			if digest, err := refreshDocumentHash(target); err == nil {
				targetIsResult = digest == resultDigest
			}
		}
	}
	if !targetIsResult {
		unchanged := (missing && recovery.TargetAbsent) || (readErr == nil && !recovery.TargetAbsent && refreshBytesHash(targetBytes) == recovery.TargetSHA256)
		if !unchanged {
			return true, refreshLocal("REFRESH_RECOVERY_INVALID", "destination was changed since refresh; refusing to overwrite it or repeat the grant", nil)
		}
		if err := protocol.WritePrivateJSON(targetPath, updated); err != nil {
			return true, refreshLocal("REFRESH_OUTPUT_PENDING", "accepted grant is saved but output replacement failed; retry locally", err)
		}
	}
	if err := os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, refreshLocal("REFRESH_OUTPUT_PENDING", "output is saved but private recovery cleanup failed; retry locally", err)
	}
	return true, nil
}
