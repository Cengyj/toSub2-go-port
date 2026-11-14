package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"tosub2-go-port/internal/accountplan"
	"tosub2-go-port/internal/store"
)

var (
	ErrSubscriptionObservationInvalid   = errors.New("未取得可确认账号身份的有效套餐结果，已保留历史信息")
	ErrSubscriptionPersistence          = errors.New("套餐查询结果保存失败，已保留历史信息")
	ErrSubscriptionPersistenceUncertain = errors.New("套餐查询结果已写入，但无法确认持久保存，请检查本地存储")
)

// SubscriptionObservation contains only an identity-verified live response.
// A nil expiry means that this plan has no reliable reported subscription date;
// it must not inherit the previous plan's date or the access-token expiry.
type SubscriptionObservation struct {
	PlanType   string
	Source     string
	ObservedAt time.Time
	AccountID  string
	ExpiresAt  *string
}

// CommitSubscription is the terminal, local-only commit of a reserved query.
// The application verifies the grant and response before calling this method;
// the manager rechecks ownership, quarantine and the exact committed file under
// the same lock used for all metadata mutations. It never starts a job.
func (l *AccountQueryLease) CommitSubscription(ctx context.Context, observation SubscriptionObservation, authorizationDigest string) (*Job, error) {
	if l == nil {
		return nil, ErrAccountQuerySuperseded
	}
	if ctx == nil {
		ctx = context.Background()
	}
	plan := accountplan.Normalize(observation.PlanType)
	if plan == "unknown" || !subscriptionPlanSource(observation.Source) || observation.ObservedAt.IsZero() || observation.ObservedAt.Year() < 1 || observation.ObservedAt.Year() > 9999 || strings.TrimSpace(observation.AccountID) == "" || !subscriptionDigestValid(authorizationDigest) {
		return nil, ErrSubscriptionObservationInvalid
	}
	expires := ""
	if observation.ExpiresAt != nil {
		var err error
		expires, err = NormalizeExpiry(*observation.ExpiresAt)
		if err != nil {
			return nil, ErrSubscriptionObservationInvalid
		}
	}
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !l.validLocked() {
		return nil, ErrAccountQuerySuperseded
	}
	j := m.jobs[l.job.ID]
	if m.totpBlockedLocked(j.Email) {
		return nil, ErrTOTPRecoveryPending
	}
	if m.store == nil || !j.ResultSaved {
		return nil, ErrAccountQuerySuperseded
	}
	digest, accountID, err := subscriptionAuthorizationBinding(m.store.Root, j.ID, j.Email)
	if err != nil || digest != strings.ToLower(authorizationDigest) || accountID != strings.TrimSpace(observation.AccountID) {
		return nil, ErrAccountQuerySuperseded
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !l.validLocked() {
		return nil, ErrAccountQuerySuperseded
	}
	before := cloneJob(j)
	observedAt := observation.ObservedAt.UTC()
	j.PlanType, j.SubscriptionExpiresAt = plan, expires
	j.SubscriptionPlanCheckedAt, j.SubscriptionPlanSource, j.SubscriptionPlanStale = &observedAt, observation.Source, false
	j.subscriptionPlanDigest, j.subscriptionPlanAccountID = digest, accountID
	j.UpdatedAt = time.Now().UTC()
	m.persistLocked()
	if m.lastPersistError != nil {
		if errors.Is(m.lastPersistError, store.ErrDurabilityUncertain) {
			return nil, ErrSubscriptionPersistenceUncertain
		}
		*j = *before
		return nil, ErrSubscriptionPersistence
	}
	return SanitizeJobForPublic(j), nil
}

func subscriptionPlanSource(source string) bool {
	return source == "chatgpt_accounts" || source == "chatgpt_subscriptions"
}

func subscriptionDigestValid(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

// The binding contains no token, cookie, password or TOTP material. A digest
// binds the observation to the exact saved authorization and the selected
// account ID prevents a different workspace from inheriting its live status.
func subscriptionAuthorizationBinding(root, id, email string) (digest, accountID string, err error) {
	data, err := readPlanExport(filepath.Join(root, id, "sub2api-import-oauth.json"))
	if err != nil {
		return "", "", err
	}
	var document struct {
		Type     string           `json:"type"`
		Accounts []map[string]any `json:"accounts"`
	}
	if json.Unmarshal(data, &document) != nil || document.Type != "sub2api-data" || len(document.Accounts) != 1 {
		return "", "", ErrSubscriptionObservationInvalid
	}
	account := document.Accounts[0]
	credentials, _ := account["credentials"].(map[string]any)
	extra, _ := account["extra"].(map[string]any)
	resolvedEmail := ""
	for _, source := range []map[string]any{credentials, extra} {
		for _, key := range []string{"email", "chatgpt_account_id", "account_id"} {
			raw, exists := source[key]
			if !exists || raw == nil {
				continue
			}
			value, ok := raw.(string)
			value = strings.TrimSpace(value)
			if !ok {
				return "", "", ErrSubscriptionObservationInvalid
			}
			if value == "" {
				continue
			}
			if key == "email" {
				if resolvedEmail != "" && !strings.EqualFold(resolvedEmail, value) {
					return "", "", ErrSubscriptionObservationInvalid
				}
				resolvedEmail = value
			} else {
				if accountID != "" && accountID != value {
					return "", "", ErrSubscriptionObservationInvalid
				}
				accountID = value
			}
		}
	}
	if strings.TrimSpace(email) == "" || !strings.EqualFold(resolvedEmail, strings.TrimSpace(email)) || accountID == "" {
		return "", "", ErrSubscriptionObservationInvalid
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), accountID, nil
}

func validSubscriptionObservation(j *Job) bool {
	return j != nil && j.SubscriptionPlanCheckedAt != nil && !j.SubscriptionPlanCheckedAt.IsZero() && subscriptionPlanSource(j.SubscriptionPlanSource) && subscriptionDigestValid(j.subscriptionPlanDigest) && j.subscriptionPlanAccountID != ""
}

// applySavedSubscriptionMetadata gives a live observation precedence only for
// the exact grant it inspected. New authorizations invalidate that precedence;
// old claims may remain a historical display but cannot regain live freshness.
func applySavedSubscriptionMetadata(root string, j *Job) {
	plan, expiry := outputPlanType(root, j.ID, j.Email), ""
	expiry, expiryFound := outputSubscriptionExpiry(root, j.ID, j.Email)
	if validSubscriptionObservation(j) {
		digest, accountID, err := subscriptionAuthorizationBinding(root, j.ID, j.Email)
		if err == nil && digest == j.subscriptionPlanDigest && accountID == j.subscriptionPlanAccountID {
			return
		}
		j.SubscriptionPlanStale = true
		if err != nil {
			// No usable replacement grant exists. Keep the previous observation
			// explicitly historical rather than attributing unrelated fallback data.
			return
		}
		if plan == "unknown" && accountID == j.subscriptionPlanAccountID {
			// The new grant has not identified its plan. Keep the complete old
			// observation historical; its date cannot come from the unknown plan.
			return
		}
		// Never reuse a previous workspace's tier or expiration for a newly
		// selected account whose export does not report those fields.
		if plan != "unknown" || accountID != j.subscriptionPlanAccountID {
			j.PlanType = plan
			// These values now come from the replacement export, not from the
			// previous live query. Its timestamp/source must not label them.
			j.SubscriptionPlanCheckedAt, j.SubscriptionPlanSource = nil, ""
			j.subscriptionPlanDigest, j.subscriptionPlanAccountID = "", ""
		}
		if plan == "unknown" {
			j.SubscriptionExpiresAt = ""
		} else if expiryFound {
			j.SubscriptionExpiresAt = expiry
		} else {
			j.SubscriptionExpiresAt = ""
		}
		return
	}
	if j.SubscriptionPlanCheckedAt != nil || j.SubscriptionPlanSource != "" {
		j.SubscriptionPlanStale = true
	}
	if j.SubscriptionPlanStale && plan == "unknown" {
		// A replacement that cleared live provenance cannot regain an
		// unconfirmed subscription date on the next restart or inspection.
		j.SubscriptionExpiresAt = ""
		return
	}
	if plan != "unknown" {
		j.PlanType = plan
	}
	if expiryFound {
		j.SubscriptionExpiresAt = expiry
	}
}

func restoreSubscriptionBinding(raw json.RawMessage, j *Job) {
	var binding struct {
		Digest    string `json:"authorizationDigest"`
		AccountID string `json:"accountId"`
	}
	if json.Unmarshal(raw, &binding) == nil && subscriptionDigestValid(binding.Digest) && strings.TrimSpace(binding.AccountID) != "" {
		j.subscriptionPlanDigest, j.subscriptionPlanAccountID = strings.ToLower(binding.Digest), strings.TrimSpace(binding.AccountID)
	}
}
