package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/protocol"
)

var (
	ErrInsightGrantUnavailable = errors.New("account query authorization is unavailable or invalid")
	ErrInsightGrantIdentity    = errors.New("account query authorization does not match the saved account identity")
	ErrInsightProxyUnavailable = errors.New("account query proxy could not be read safely")
	ErrInsightRecoveryPending  = errors.New("account query is paused until saved token rotation is finalized")
)

// AccountInsightGrant is a private, transient snapshot. Keep its lease until
// the query and guarded snapshot commit finish; never serialize this object.
type AccountInsightGrant struct {
	Lease          *jobs.AccountQueryLease     `json:"-"`
	Credentials    protocol.InsightCredentials `json:"-"`
	ProxyURL       string                      `json:"-"`
	ProxyOverride  bool                        `json:"-"`
	IdentityKey    string                      `json:"-"`
	TokenExpiresAt int64                       `json:"-"`
	// ExpiresAt is the earliest known access-token or export expiry. A nil
	// value means the committed grant does not report an expiry.
	ExpiresAt      *time.Time `json:"-"`
	manager        *jobs.Manager
	root, id       string
	digest         [32]byte
	proxyFromVault bool
}

func (g *AccountInsightGrant) Context() context.Context { return g.Lease.Context() }
func (g *AccountInsightGrant) Release() {
	if g != nil {
		g.Lease.Release()
	}
}

// Validate rejects replaced grants and proxy changes as well as manager-side
// invalidation. It must run immediately before publishing a query result.
func (g *AccountInsightGrant) Validate() error {
	if g == nil || !g.Lease.Valid() {
		return jobs.ErrAccountQuerySuperseded
	}
	data, err := readInsightGrant(g.Context(), g.root, g.id)
	if err != nil || sha256.Sum256(data) != g.digest {
		return jobs.ErrAccountQuerySuperseded
	}
	if g.proxyFromVault {
		saved, err := g.manager.LoadCredentialsContext(g.Context(), g.id)
		if err != nil || saved.ProxyURL != g.ProxyURL {
			return jobs.ErrAccountQuerySuperseded
		}
	}
	if !g.Lease.Valid() {
		return jobs.ErrAccountQuerySuperseded
	}
	return nil
}

// LoadAccountInsightGrant reads a committed main OAuth export. It never starts
// authentication, refreshes tokens, probes a proxy, or uses pending grants.
func LoadAccountInsightGrant(ctx context.Context, root string, manager *jobs.Manager, id string) (_ *AccountInsightGrant, err error) {
	if manager == nil {
		return nil, ErrInsightGrantUnavailable
	}
	lease, err := manager.ReserveAccountQuery(ctx, id)
	if err != nil {
		if errors.Is(err, jobs.ErrAccountQueryProxyInvalid) {
			return nil, ErrInsightProxyUnavailable
		}
		return nil, err
	}
	defer func() {
		if err != nil {
			lease.Release()
		}
	}()
	job := lease.Job()
	data, err := readInsightGrant(lease.Context(), root, id)
	if err != nil {
		return nil, err
	}
	var document struct {
		Type     string           `json:"type"`
		Version  int              `json:"version"`
		Accounts []map[string]any `json:"accounts"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var trailing any
	if decoder.Decode(&document) != nil || decoder.Decode(&trailing) != io.EOF || document.Type != "sub2api-data" || document.Version < 0 || document.Version > 1 || len(document.Accounts) != 1 {
		return nil, ErrInsightGrantUnavailable
	}
	account := document.Accounts[0]
	credentials, _ := account["credentials"].(map[string]any)
	extra, _ := account["extra"].(map[string]any)
	access, _ := credentials["access_token"].(string)
	if strings.TrimSpace(access) == "" {
		return nil, ErrInsightGrantUnavailable
	}
	method, err := jobs.AuthorizationMethodForClientIdentity(credentials, extra)
	if err != nil || method == "" || method != job.AuthorizationMethod {
		return nil, ErrInsightGrantIdentity
	}
	email, emailOK := insightIdentityField([]map[string]any{credentials, extra}, true, "email")
	accountID, accountOK := insightIdentityField([]map[string]any{credentials, extra}, false, "chatgpt_account_id", "account_id")
	if !emailOK || !accountOK || email == "" || accountID == "" || !strings.EqualFold(email, strings.TrimSpace(job.Email)) {
		return nil, ErrInsightGrantIdentity
	}
	idToken, _ := credentials["id_token"].(string)
	metadata := protocol.ExtractOAuthTokenMetadata(protocol.TokenSet{AccessToken: access, IDToken: idToken})
	if metadata.ChatGPTAccountID != "" && metadata.ChatGPTAccountID != accountID || metadata.Email != "" && !strings.EqualFold(metadata.Email, email) {
		return nil, ErrInsightGrantIdentity
	}
	accessClaims, _ := protocol.DecodeJWTClaims(access)
	if tokenMethod, tokenErr := jobs.AuthorizationMethodForClientIdentity(accessClaims, nil); tokenErr != nil || tokenMethod != "" && tokenMethod != method {
		return nil, ErrInsightGrantIdentity
	}
	plan := accountplan.FromExplicitAccount(account)
	if plan == "unknown" {
		plan = accountplan.Normalize(metadata.PlanType)
	}
	g := &AccountInsightGrant{Lease: lease, manager: manager, root: root, id: id, digest: sha256.Sum256(data), TokenExpiresAt: metadata.ExpiresAt,
		ExpiresAt:   insightGrantExpiry(credentials["expires_at"], metadata.ExpiresAt),
		Credentials: protocol.InsightCredentials{AccessToken: access, AccountID: accountID, Email: email, AuthorizationMethod: method, PlanType: plan}}
	g.ProxyURL, g.ProxyOverride = lease.ProxyInput()
	if !g.ProxyOverride && job.ProxyConfigured {
		saved, loadErr := manager.LoadCredentialsContext(lease.Context(), id)
		if err := lease.Context().Err(); err != nil {
			return nil, err
		}
		if loadErr != nil || strings.TrimSpace(saved.ProxyURL) == "" {
			return nil, ErrInsightProxyUnavailable
		}
		g.ProxyURL, g.ProxyOverride, g.proxyFromVault = saved.ProxyURL, true, true
	}
	identity, _ := json.Marshal([]any{"main", id, method, accountID, email, hex.EncodeToString(g.digest[:]), g.ProxyOverride, g.ProxyURL})
	identityDigest := sha256.Sum256(identity)
	g.IdentityKey = hex.EncodeToString(identityDigest[:])
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return g, nil
}

func insightIdentityField(sources []map[string]any, fold bool, keys ...string) (string, bool) {
	result := ""
	for _, fields := range sources {
		for _, key := range keys {
			value, exists := fields[key]
			if !exists || value == nil {
				continue
			}
			text, ok := value.(string)
			if !ok {
				return "", false
			}
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			if result != "" && result != text && !(fold && strings.EqualFold(result, text)) {
				return "", false
			}
			result = text
		}
	}
	return result, true
}

func insightGrantExpiry(exported any, tokenExpiry int64) *time.Time {
	var exportExpiry int64
	switch value := exported.(type) {
	case json.Number:
		exportExpiry, _ = value.Int64()
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
			exportExpiry = parsed.Unix()
		} else {
			exportExpiry, _ = strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		}
	}
	var expires *time.Time
	for _, seconds := range []int64{exportExpiry, tokenExpiry} {
		if seconds <= 0 || seconds > 253402300799 {
			continue
		}
		value := time.Unix(seconds, 0).UTC()
		if expires == nil || value.Before(*expires) {
			expires = &value
		}
	}
	return expires
}

func readInsightGrant(ctx context.Context, root, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if root == "" || id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, "/\\") {
		return nil, ErrInsightGrantUnavailable
	}
	directory := filepath.Join(root, id)
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() {
		return nil, ErrInsightGrantUnavailable
	}
	dirInfo, err := os.Lstat(directory)
	if err != nil || !dirInfo.IsDir() {
		return nil, ErrInsightGrantUnavailable
	}
	if _, err := os.Lstat(filepath.Join(directory, ".sub2api-import-oauth.json.refresh-grant-recovery.json")); !errors.Is(err, os.ErrNotExist) {
		return nil, ErrInsightRecoveryPending
	}
	path := filepath.Join(directory, "sub2api-import-oauth.json")
	info, err := os.Lstat(path)
	const maxBytes = 2 << 20
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, ErrInsightGrantUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrInsightGrantUnavailable
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || privatefile.Verify(file) != nil {
		return nil, ErrInsightGrantUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || len(data) > maxBytes {
		return nil, ErrInsightGrantUnavailable
	}
	currentRoot, rootErr := os.Lstat(root)
	currentDir, dirErr := os.Lstat(directory)
	if rootErr != nil || dirErr != nil || !currentRoot.IsDir() || !currentDir.IsDir() || !os.SameFile(rootInfo, currentRoot) || !os.SameFile(dirInfo, currentDir) {
		return nil, ErrInsightGrantUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}
