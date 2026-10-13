package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"tosub2-go-port/internal/protocol"
)

// Serialize lookup and writes for one backend across clients. Console handlers
// construct a new Client per request, so a mutex on Client would still allow
// two simultaneous uploads to create the same previously absent account.
var uploadLocks = struct {
	sync.Mutex
	entries map[string]*backendUploadLock
}{entries: map[string]*backendUploadLock{}}

type backendUploadLock struct {
	gate chan struct{}
	refs int
}

func lockBackendUpload(ctx context.Context, backend string) (func(), error) {
	uploadLocks.Lock()
	entry := uploadLocks.entries[backend]
	if entry == nil {
		entry = &backendUploadLock{gate: make(chan struct{}, 1)}
		uploadLocks.entries[backend] = entry
	}
	entry.refs++
	uploadLocks.Unlock()
	releaseRef := func() {
		uploadLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(uploadLocks.entries, backend)
		}
		uploadLocks.Unlock()
	}
	select {
	case entry.gate <- struct{}{}:
		return func() { <-entry.gate; releaseRef() }, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}

// Only authorization data may replace remote credentials. In particular,
// model_mapping and other provider settings in the import are not applied to
// existing accounts. The requested timezone and OAuth client identity in extra
// are updated alongside authorization; other remote settings are preserved.
var oauthCredentialFields = []string{
	"access_token", "refresh_token", "id_token", "email", "chatgpt_account_id",
	"account_id", "chatgpt_user_id", "user_id", "client_id", "token_type", "scope",
	"expires_at", "expires_in", "organization_id", "plan_type", "subscription_expires_at",
}

func oauthClientID(credentials, extra map[string]any) (string, error) {
	client := strings.TrimSpace(stringValue(credentials["client_id"]))
	metadata := strings.TrimSpace(stringValue(extra["client_id"]))
	if client != "" && metadata != "" && client != metadata {
		return "", errors.New("Sub2API account has conflicting OAuth client identities")
	}
	if client == "" {
		client = metadata
	}
	return client, nil
}

func mergeOAuthCredentials(remote, extra, incoming map[string]any) map[string]any {
	merged := cloneMap(remote)
	client := strings.TrimSpace(stringValue(incoming["client_id"]))
	previous, err := oauthClientID(remote, extra)
	accountChanged := oauthAccountMetadataChanged(remote, extra, incoming)
	if client != "" && (client != previous || err != nil) {
		// A different client owns a different grant. Do not leave the old ID
		// token, expiry or account identifiers attached to the new access token.
		for _, field := range oauthCredentialFields {
			if !accountChanged && (field == "organization_id" || field == "plan_type" || field == "subscription_expires_at") {
				continue
			}
			delete(merged, field)
		}
	}
	if accountChanged {
		for _, field := range []string{"account_id", "chatgpt_account_id", "chatgpt_user_id", "user_id", "organization_id", "plan_type", "subscription_expires_at"} {
			delete(merged, field)
		}
	}
	if oauthPlanChanged(remote, extra, incoming) && stringValue(incoming["subscription_expires_at"]) == "" {
		delete(merged, "subscription_expires_at")
	}
	if access := stringValue(incoming["access_token"]); access != "" && access != stringValue(remote["access_token"]) {
		// Optional grant metadata describes the old access token, not settings.
		// BPS can legitimately return a new grant without an ID token.
		for _, field := range []string{"id_token", "expires_at", "expires_in", "scope", "token_type"} {
			if _, present := incoming[field]; !present {
				delete(merged, field)
			}
		}
	}
	for _, field := range oauthCredentialFields {
		if value, exists := incoming[field]; exists {
			if value == "" {
				if field == "id_token" {
					delete(merged, field)
				}
				continue
			}
			merged[field] = value
		}
	}
	protocol.ApplyBPSCredentialSettings(merged, client)
	return merged
}

type oauthUploadAccount struct {
	email   string
	account map[string]any
}

type oauthUploadUpdate struct {
	id          int64
	credentials map[string]any
	extra       map[string]any
}

func decodeOAuthUpload(payload any) ([]oauthUploadAccount, int, error) {
	data, err := json.Marshal(payload)
	if err != nil || len(data) > 16<<20 {
		return nil, 0, errors.New("invalid Sub2API upload payload")
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if json.Unmarshal(data, &doc) != nil || len(doc.Accounts) == 0 || len(doc.Accounts) > 10000 {
		return nil, 0, errors.New("Sub2API upload requires OpenAI OAuth accounts")
	}
	accounts := make([]oauthUploadAccount, 0, len(doc.Accounts))
	byEmail := map[string]int{}
	skipped := 0
	for _, account := range doc.Accounts {
		if stringValue(account["platform"]) != "openai" || stringValue(account["type"]) != "oauth" {
			return nil, 0, errors.New("Sub2API upload accepts only OpenAI OAuth accounts")
		}
		credentials, ok := account["credentials"].(map[string]any)
		if !ok || strings.TrimSpace(stringValue(credentials["access_token"])) == "" {
			return nil, 0, errors.New("Sub2API upload account lacks identity or OAuth credentials")
		}
		extra := cloneMap(account["extra"])
		if err := normalizeOAuthAuthorization(credentials, extra); err != nil {
			return nil, 0, err
		}
		account["extra"] = extra
		email := AccountEmail(account)
		if email == "" {
			return nil, 0, errors.New("Sub2API upload account lacks identity or OAuth credentials")
		}
		for _, key := range oauthCredentialFields {
			value, exists := credentials[key]
			if !exists {
				continue
			}
			if key == "expires_at" || key == "expires_in" {
				if _, valid := nonnegativeInteger(value); !valid {
					return nil, 0, errors.New("Sub2API upload has invalid OAuth expiry")
				}
				continue
			}
			if _, ok := value.(string); !ok {
				return nil, 0, errors.New("Sub2API upload has invalid OAuth credentials")
			}
		}
		credentials["email"] = email
		// Keep the last authorization for duplicate local records, but never
		// send multiple writes or new records for one normalized identity.
		if previous, exists := byEmail[email]; exists {
			accounts[previous] = oauthUploadAccount{email: email, account: account}
			skipped++
			continue
		}
		byEmail[email] = len(accounts)
		accounts = append(accounts, oauthUploadAccount{email: email, account: account})
	}
	return accounts, skipped, nil
}

func (c *Client) upsertOAuthAccounts(ctx context.Context, payload any, key string) (any, error) {
	defaultTimezone, err := NormalizeOpenAIRequestTimezone(c.cfg.OpenAIRequestTimezone)
	if err != nil {
		return nil, err
	}
	accounts, skipped, err := decodeOAuthUpload(payload)
	if err != nil {
		return nil, err
	}
	for _, local := range accounts {
		extra := cloneMap(local.account["extra"])
		timezone := defaultTimezone
		if value, exists := extra["openai_request_timezone"]; exists {
			raw, ok := value.(string)
			if !ok {
				return nil, ErrInvalidOpenAIRequestTimezone
			}
			timezone, err = NormalizeOpenAIRequestTimezone(raw)
			if err != nil {
				return nil, err
			}
		}
		if c.timezoneConfigured {
			timezone = defaultTimezone
		}
		extra["openai_request_timezone"] = timezone
		local.account["extra"] = extra
	}
	if ctx == nil {
		ctx = context.Background()
	}
	backend, err := normalizeBaseURL(c.cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	unlock, err := lockBackendUpload(ctx, backend)
	if err != nil {
		return nil, err
	}
	defer unlock()
	remote, err := c.completeUploadAccountList(ctx)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, account := range accounts {
		wanted[account.email] = true
	}
	matches := map[string][]map[string]any{}
	for _, account := range remote {
		platform, kind := stringValue(account["platform"]), stringValue(account["type"])
		if (platform != "" && platform != "openai") || (kind != "" && kind != "oauth") {
			continue
		}
		email := AccountEmail(account)
		// A renamed/redacted list record may contain no email. Resolve its
		// detail rather than treating an uncertain identity as non-existent.
		if email == "" || platform == "" || kind == "" {
			account, err = c.uploadAccountDetail(ctx, intValue(account["id"]))
			if err != nil {
				return nil, err
			}
			platform, kind, email = stringValue(account["platform"]), stringValue(account["type"]), AccountEmail(account)
			if platform != "openai" || kind != "oauth" {
				if platform == "" || kind == "" {
					return nil, errors.New("Sub2API account identity is incomplete; upload stopped")
				}
				continue
			}
			if email == "" {
				return nil, errors.New("Sub2API account email is unavailable; upload stopped")
			}
		}
		if wanted[email] {
			matches[email] = append(matches[email], account)
		}
	}
	updates := []oauthUploadUpdate{}
	newAccounts := []any{}
	// Resolve and validate every target before issuing any mutation. Lookup,
	// ambiguity or stale-identity failures must never fall back to creation.
	for _, local := range accounts {
		matched := matches[local.email]
		if len(matched) > 1 {
			return nil, errors.New("Sub2API has multiple matching OAuth accounts; upload stopped")
		}
		if len(matched) == 0 {
			newAccounts = append(newAccounts, local.account)
			continue
		}
		id := intValue(matched[0]["id"])
		detail, err := c.uploadAccountDetail(ctx, id)
		if err != nil {
			return nil, err
		}
		if stringValue(detail["platform"]) != "openai" || stringValue(detail["type"]) != "oauth" || AccountEmail(detail) != local.email {
			return nil, errors.New("Sub2API account identity changed; upload stopped")
		}
		remoteCredentials, ok := detail["credentials"].(map[string]any)
		if !ok {
			return nil, errors.New("Sub2API account credentials are unavailable; upload stopped")
		}
		incoming := local.account["credentials"].(map[string]any)
		remoteExtra, _ := detail["extra"].(map[string]any)
		merged := mergeOAuthCredentials(remoteCredentials, remoteExtra, incoming)
		extra := mergeOAuthExtra(remoteCredentials, remoteExtra, incoming)
		merged["email"] = local.email
		extra["openai_request_timezone"] = local.account["extra"].(map[string]any)["openai_request_timezone"]
		updates = append(updates, oauthUploadUpdate{id: id, credentials: merged, extra: extra})
	}
	result := map[string]any{
		"ok": true, "created": float64(0), "updated": float64(0), "success": float64(0),
		"failed": float64(0), "skipped": float64(skipped), "total": float64(len(accounts) + skipped),
		"account_ids": []any{},
	}
	for _, update := range updates {
		if _, err := c.Update(ctx, strconv.FormatInt(update.id, 10), map[string]any{"credentials": update.credentials, "extra": update.extra}); err != nil {
			result["ok"], result["failed"] = false, float64(1)
			return result, err
		}
		result["updated"] = result["updated"].(float64) + 1
		result["success"] = result["success"].(float64) + 1
		result["account_ids"] = append(result["account_ids"].([]any), update.id)
	}
	if len(newAccounts) > 0 {
		var raw any
		headers := make(http.Header)
		if validHeaderToken(key) {
			headers.Set("Idempotency-Key", key)
		}
		err := c.requestWithOptions(ctx, http.MethodPost, "api/v1/admin/accounts/batch", map[string]any{"accounts": newAccounts}, &raw, requestOptions{Headers: headers})
		if err != nil {
			result["ok"] = false
			return result, err
		}
		created, failed, ids, err := uploadCreationResult(raw, len(newAccounts))
		result["created"], result["failed"] = float64(created), float64(failed)
		result["success"] = result["success"].(float64) + float64(created)
		result["account_ids"] = append(result["account_ids"].([]any), ids...)
		if err != nil {
			result["ok"] = false
			return result, err
		}
	}
	return result, nil
}

func (c *Client) uploadAccountDetail(ctx context.Context, id int64) (map[string]any, error) {
	value, err := c.Account(ctx, strconv.FormatInt(id, 10))
	if err != nil {
		return nil, err
	}
	detail, ok := value.(map[string]any)
	if detail["ok"] == false || detail["success"] == false {
		return nil, errors.New("Sub2API account detail was rejected; upload stopped")
	}
	if nested, nestedOK := detail["data"].(map[string]any); nestedOK {
		detail = nested
	}
	if !ok || id <= 0 || intValue(detail["id"]) != id {
		return nil, errors.New("Sub2API account detail is invalid; upload stopped")
	}
	return detail, nil
}

// completeUploadAccountList is stricter than the monitor's compatibility
// parser: creation requires evidence that every page was actually retrieved.
func (c *Client) completeUploadAccountList(ctx context.Context) ([]map[string]any, error) {
	accounts := []map[string]any{}
	seenIDs := map[int64]bool{}
	expectedTotal, expectedPages := int64(-1), int64(-1)
	for page := int64(1); page <= 1000; page++ {
		var value any
		if err := c.request(ctx, http.MethodGet, fmt.Sprintf("api/v1/admin/accounts?page=%d&page_size=100&platform=openai&type=oauth", page), nil, &value); err != nil {
			return nil, err
		}
		items, metadata, err := uploadListPage(value)
		if err != nil {
			return nil, err
		}
		if reportedPage, exists := metadata["page"]; exists && reportedPage != page {
			return nil, errors.New("Sub2API account pagination is inconsistent; upload stopped")
		}
		if total, exists := metadata["total"]; exists {
			if expectedTotal >= 0 && total != expectedTotal {
				return nil, errors.New("Sub2API account total changed during lookup; upload stopped")
			}
			expectedTotal = total
		}
		pages, reportedPages := metadata["pages"]
		if reportedPages {
			if pages > 1000 || (pages == 0 && len(items) != 0) || (expectedPages >= 0 && pages != expectedPages) {
				return nil, errors.New("Sub2API account pagination is incomplete; upload stopped")
			}
			expectedPages = pages
		}
		for _, item := range items {
			account, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("Sub2API account list is invalid; upload stopped")
			}
			id := intValue(account["id"])
			if id <= 0 || seenIDs[id] {
				return nil, errors.New("Sub2API account list contains invalid or repeated IDs; upload stopped")
			}
			seenIDs[id] = true
			accounts = append(accounts, account)
		}
		if expectedTotal >= 0 && int64(len(accounts)) > expectedTotal {
			return nil, errors.New("Sub2API account list exceeds reported total; upload stopped")
		}
		complete := len(items) < 100
		if expectedPages >= 0 {
			complete = page >= expectedPages
			if !complete && len(items) == 0 {
				return nil, errors.New("Sub2API account page is missing; upload stopped")
			}
		} else if expectedTotal >= 0 {
			complete = int64(len(accounts)) == expectedTotal
			if !complete && len(items) == 0 {
				return nil, errors.New("Sub2API account list is incomplete; upload stopped")
			}
		}
		if complete {
			if expectedTotal >= 0 && int64(len(accounts)) != expectedTotal {
				return nil, errors.New("Sub2API account list is incomplete; upload stopped")
			}
			return accounts, nil
		}
	}
	return nil, errors.New("Sub2API account pagination limit reached; upload stopped")
}

func uploadListPage(value any) ([]any, map[string]int64, error) {
	metadata := map[string]int64{}
	data := value
	if envelope, ok := value.(map[string]any); ok {
		if envelope["ok"] == false || envelope["success"] == false {
			return nil, nil, errors.New("Sub2API account lookup was rejected; upload stopped")
		}
		if nested, exists := envelope["data"]; exists {
			data = nested
		}
		for _, key := range []string{"page", "pages", "total"} {
			if value, exists := envelope[key]; exists {
				n, valid := nonnegativeInteger(value)
				if !valid {
					return nil, nil, errors.New("Sub2API pagination metadata is invalid; upload stopped")
				}
				metadata[key] = n
			}
		}
	}
	if object, ok := data.(map[string]any); ok {
		if object["ok"] == false || object["success"] == false {
			return nil, nil, errors.New("Sub2API account lookup was rejected; upload stopped")
		}
		for _, key := range []string{"page", "pages", "total"} {
			if value, exists := object[key]; exists {
				n, valid := nonnegativeInteger(value)
				if !valid {
					return nil, nil, errors.New("Sub2API pagination metadata is invalid; upload stopped")
				}
				if prior, exists := metadata[key]; exists && prior != n {
					return nil, nil, errors.New("Sub2API pagination metadata is inconsistent; upload stopped")
				}
				metadata[key] = n
			}
		}
		data = object["items"]
	}
	items, ok := data.([]any)
	if !ok {
		return nil, nil, errors.New("Sub2API account list is unavailable; upload stopped")
	}
	return items, metadata, nil
}

func nonnegativeInteger(value any) (int64, bool) {
	if value == float64(0) || value == int(0) || value == int64(0) || value == json.Number("0") || value == "0" {
		return 0, true
	}
	n := intValue(value)
	return n, n > 0
}

func uploadCreationResult(value any, requested int) (int, int, []any, error) {
	created, failed := requested, 0
	for {
		object, ok := value.(map[string]any)
		if !ok {
			break
		}
		if object["ok"] == false || object["success"] == false {
			return 0, requested, nil, errors.New("Sub2API rejected new account upload")
		}
		if nested, exists := object["data"]; exists {
			value = nested
			continue
		}
		for _, key := range []string{"created", "success_count", "success", "imported", "count"} {
			if count, valid := nonnegativeInteger(object[key]); valid {
				created = int(count)
				break
			}
		}
		for _, key := range []string{"failed", "failure_count"} {
			if count, valid := nonnegativeInteger(object[key]); valid {
				failed = int(count)
				break
			}
		}
		ids := []any{}
		for _, key := range []string{"account_ids", "ids"} {
			if list, ok := object[key].([]any); ok {
				for _, value := range list {
					if id := intValue(value); id > 0 {
						ids = append(ids, id)
					}
				}
				break
			}
		}
		if created > requested || failed > requested || created+failed > requested {
			return 0, 0, nil, errors.New("Sub2API returned invalid upload counts")
		}
		if failed > 0 || created < requested {
			if failed == 0 {
				failed = requested - created
			}
			return created, failed, ids, errors.New("Sub2API did not accept all new accounts")
		}
		return created, failed, ids, nil
	}
	return created, failed, nil, nil
}
