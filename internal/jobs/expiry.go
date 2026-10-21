package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/protocol"
	"tosub2-go-port/internal/store"
)

// NormalizeExpiry accepts an absolute RFC3339 timestamp. Human date formats
// and time zones belong at the input boundary, never in stored account data.
func NormalizeExpiry(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	expires, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || expires.IsZero() || expires.UTC().Year() < 1 || expires.UTC().Year() > 9999 {
		return "", errors.New("到期时间必须是包含时区的有效日期时间")
	}
	return expires.UTC().Format(time.RFC3339Nano), nil
}

// EffectiveExpiry is the single precedence rule for local account expiry.
// Access-token expiry is intentionally unrelated to this display metadata.
func EffectiveExpiry(job *Job) (expiresAt, source string) {
	if job != nil {
		if value, err := NormalizeExpiry(job.ManualExpiresAt); err == nil && value != "" {
			return value, "manual"
		}
		if value, err := NormalizeExpiry(job.SubscriptionExpiresAt); err == nil && value != "" {
			return value, "subscription"
		}
	}
	return "", "unknown"
}

func manualExpiryForValues(current string, values map[string]any) (string, bool, error) {
	value, present := values["manualExpiresAt"]
	if !present {
		return current, false, nil
	}
	text, ok := value.(string)
	if !ok {
		return "", true, errors.New("手工到期时间必须是日期时间字符串")
	}
	normalized, err := NormalizeExpiry(text)
	return normalized, true, err
}

func onlyManualExpiryValues(values map[string]any) bool {
	_, present := values["manualExpiresAt"]
	return present && len(values) == 1
}

// SetManualExpiryContext edits local metadata only. A nil value explicitly
// clears the override. Running/queued authentication and credentials are not
// touched. A definite write failure restores the prior value; uncertainty
// after replacement keeps the visible new value and reports uncertain storage.
func (m *Manager) SetManualExpiryContext(ctx context.Context, id string, value *time.Time) (*Job, error) {
	expires := ""
	if value != nil {
		var err error
		expires, err = NormalizeExpiry(value.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.setManualExpiryLocked(ctx, id, expires)
}

func (m *Manager) setManualExpiryLocked(ctx context.Context, id, expires string) (*Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.closing || m.rootCtx.Err() != nil {
		return nil, errors.New("job manager is shutting down")
	}
	job, ok := m.jobs[id]
	if !ok {
		return nil, osErrNotFound(id)
	}
	if m.deleting[emailKey(job.Email)] || m.importing[emailKey(job.Email)] {
		return nil, errors.New("account is being deleted or restored")
	}
	// An explicit same-value retry must flush any unresolved persistence error.
	if job.ManualExpiresAt == expires && m.lastPersistError == nil {
		return cloneJob(job), nil
	}
	before := cloneJob(job)
	job.ManualExpiresAt, job.UpdatedAt = expires, time.Now().UTC()
	m.persistLocked()
	if err := applyManualExpiryCommitResult(job, before, m.lastPersistError); err != nil {
		return nil, err
	}
	return cloneJob(job), nil
}

// Pure metadata does not admit execution. Keep an already visible replacement
// on directory-sync failure, otherwise a later unrelated write would silently
// replace it with the old date. Its crash durability remains explicitly unknown.
func applyManualExpiryCommitResult(job, before *Job, err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrDurabilityUncertain) {
		*job = *before
	}
	return admissionPersistenceError(err)
}

// outputSubscriptionExpiry reads provider subscription metadata from the
// matching account. Older exports may carry it only in their saved JWT claims;
// inspecting those claims never refreshes or rewrites the grant. found separates
// absent/unreadable output from a current export with unknown subscription expiry.
func outputSubscriptionExpiry(root, id, email string) (expires string, found bool) {
	for _, name := range []string{"sub2api-import-oauth.json", "sub2api-data.json"} {
		data, err := readPlanExport(filepath.Join(root, id, name))
		if err != nil {
			continue
		}
		var document struct {
			Type     string           `json:"type"`
			Accounts []map[string]any `json:"accounts"`
		}
		if json.Unmarshal(data, &document) != nil || document.Type != "sub2api-data" {
			continue
		}
		for _, account := range document.Accounts {
			credentials, _ := account["credentials"].(map[string]any)
			extra, _ := account["extra"].(map[string]any)
			accountEmail, _ := credentials["email"].(string)
			if accountEmail == "" {
				accountEmail, _ = extra["email"].(string)
			}
			if accountEmail == "" {
				accountName, _ := account["name"].(string)
				accountEmail = strings.TrimPrefix(accountName, "oauth---")
			}
			if strings.TrimSpace(email) == "" || !strings.EqualFold(strings.TrimSpace(accountEmail), strings.TrimSpace(email)) {
				continue
			}
			for _, fields := range []map[string]any{credentials, extra} {
				if value, present := fields["subscription_expires_at"]; present {
					text, _ := value.(string)
					normalized, _ := NormalizeExpiry(text)
					return normalized, true
				}
			}
			return savedClaimSubscriptionExpiry(account, credentials, extra), true
		}
	}
	return "", false
}

func savedClaimSubscriptionExpiry(account, credentials, extra map[string]any) string {
	access, _ := credentials["access_token"].(string)
	idToken, _ := credentials["id_token"].(string)
	metadata := protocol.ExtractOAuthTokenMetadata(protocol.TokenSet{AccessToken: access, IDToken: idToken})
	// Explicit export metadata can be newer than its tokens. Do not associate
	// an old workspace or tier's deadline with the selected account.
	for _, fields := range []map[string]any{credentials, extra} {
		for _, key := range []string{"chatgpt_account_id", "account_id"} {
			if expected, _ := fields[key].(string); strings.TrimSpace(expected) != "" && strings.TrimSpace(expected) != metadata.ChatGPTAccountID {
				return ""
			}
		}
	}
	exportedPlan, claimPlan := accountplan.FromExplicitAccount(account), accountplan.Normalize(metadata.PlanType)
	if exportedPlan != "unknown" && claimPlan != "unknown" && exportedPlan != claimPlan {
		return ""
	}
	normalized, _ := NormalizeExpiry(metadata.SubscriptionExpiresAt)
	return normalized
}
