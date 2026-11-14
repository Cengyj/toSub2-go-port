package accountquality

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/privatefile"
)

const probeHistoryLimit = 5

// ProbeHistoryEntry contains completed display evidence only. Drawings, model
// responses and authorization never enter this independent history.
type ProbeHistoryEntry struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	Verdict    string    `json:"verdict,omitempty"`
	Score      *int      `json:"score,omitempty"`
	Total      int       `json:"total,omitempty"`
	Model      string    `json:"model,omitempty"`
	FinishedAt time.Time `json:"finishedAt"`
	ErrorCode  string    `json:"errorCode,omitempty"`
	HTTPStatus int       `json:"httpStatus,omitempty"`
}

type ProbeHistory []ProbeHistoryEntry

// Malformed optional display metadata cannot invalidate existing scoring or
// upload-policy evidence in the containing snapshot.
func (h *ProbeHistory) UnmarshalJSON(data []byte) error {
	var entries []json.RawMessage
	*h = nil
	if json.Unmarshal(data, &entries) != nil {
		return nil
	}
	for _, raw := range entries {
		var entry ProbeHistoryEntry
		if json.Unmarshal(raw, &entry) == nil {
			*h = append(*h, entry)
		}
	}
	*h = normalizeProbeHistory(*h)
	return nil
}

func historyIdentifier(value string) bool {
	if value == "" || len(value) > 160 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func historyModel(value string) string {
	if len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._:/", r)) {
			return ""
		}
	}
	return value
}

func normalizeProbeHistory(history ProbeHistory) ProbeHistory {
	var out ProbeHistory
nextItem:
	for _, item := range history {
		if !historyIdentifier(item.ID) || (item.Kind != "candy" && item.Kind != "pelican") || (item.Status != "success" && item.Status != "failed") || item.FinishedAt.IsZero() || item.FinishedAt.Year() < 1970 || item.FinishedAt.Year() > 9999 || item.FinishedAt.After(time.Now().Add(time.Minute)) {
			continue
		}
		item.Model = historyModel(item.Model)
		if item.HTTPStatus < 100 || item.HTTPStatus > 599 {
			item.HTTPStatus = 0
		}
		if item.Status == "success" {
			if item.HTTPStatus != 0 && (item.HTTPStatus < 200 || item.HTTPStatus >= 300) {
				continue
			}
			item.ErrorCode = ""
			if item.Kind == "candy" {
				if item.Score == nil || item.Total != modelprobe.TotalQuestions || *item.Score < 0 || *item.Score > item.Total || (item.Verdict != "normal" && item.Verdict != "degraded") {
					continue
				}
				if (item.Verdict == "normal") != (*item.Score == item.Total) {
					continue
				}
				score := *item.Score
				item.Score = &score
			} else {
				item.Score, item.Total, item.Verdict = nil, 0, ""
			}
		} else {
			item.Score, item.Total, item.Verdict = nil, 0, ""
			item.ErrorCode = modelprobe.DiagnoseFailure(&modelprobe.Error{Code: item.ErrorCode, HTTPStatus: item.HTTPStatus}).Code
		}
		for i, old := range out {
			if old.Kind == item.Kind && old.ID == item.ID {
				if old.FinishedAt.After(item.FinishedAt) {
					continue nextItem
				}
				out = append(out[:i], out[i+1:]...)
				break
			}
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].FinishedAt.Before(out[j].FinishedAt) })
	if len(out) > probeHistoryLimit {
		out = out[len(out)-probeHistoryLimit:]
	}
	return out
}

func candyHistoryEntry(attempt Attempt, result *modelprobe.Result, model string) *ProbeHistoryEntry {
	if attempt.CheckID == "" || attempt.FinishedAt == nil || attempt.FinishedAt.IsZero() || attempt.StartedAt == nil || attempt.FinishedAt.Before(*attempt.StartedAt) {
		return nil
	}
	if attempt.HTTPStatus == 0 {
		switch attempt.ErrorCode {
		case "authorization_unavailable", "authorization_expired", "invalid_credentials", "invalid_options", "proxy_selection_required", "transport_unavailable", "unsafe_transport", "unsupported", "settings_unavailable", "account_busy", "closed", "superseded":
			return nil
		}
	}
	item := ProbeHistoryEntry{ID: "candy-" + digest(attempt.CheckID), Kind: "candy", Status: attempt.Status, Model: model, FinishedAt: *attempt.FinishedAt, ErrorCode: attempt.ErrorCode, HTTPStatus: attempt.HTTPStatus}
	if attempt.Status == "success" && result != nil && result.RuleVersion == modelprobe.RuleVersion {
		score := result.Score
		item.Score, item.Total, item.Verdict, item.Model = &score, result.Total, result.Verdict, result.Model
	}
	normalized := normalizeProbeHistory(ProbeHistory{item})
	if len(normalized) != 1 {
		return nil
	}
	return &normalized[0]
}

func backfillProbeHistory(snapshot *Snapshot, now time.Time) {
	if len(snapshot.ProbeHistory) != 0 {
		return
	}
	attempt := snapshot.LastAttempt
	// Legacy running markers, admission failures and unknown outcomes cannot
	// establish that a probe completed. Backfill only explicit response evidence.
	if attempt.FinishedAt == nil || attempt.FinishedAt.After(now.Add(time.Minute)) || (attempt.Status != "success" && (attempt.Status != "failed" || attempt.HTTPStatus < 100 || attempt.HTTPStatus > 599)) {
		return
	}
	if item := candyHistoryEntry(attempt, snapshot.LastResult, ""); item != nil {
		snapshot.ProbeHistory = ProbeHistory{*item}
	}
}

// RecordProbeHistory appends a manual sample under the same account revision
// and live grant used for its request. It never changes scoring, cooldown,
// LastAttempt, LastResult, or upload eligibility. Failure is display-only.
func (s *Service) RecordProbeHistory(ctx context.Context, id, accountRevision string, validate func() error, item ProbeHistoryEntry) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || validate == nil || item.Kind != "pelican" || len(normalizeProbeHistory(ProbeHistory{item})) != 1 {
		return ErrSuperseded
	}
	if err := s.lockWrite(ctx); err != nil {
		return err
	}
	defer s.unlockWrite()
	current := func() bool {
		account, exists := accountMap(s.provider.Accounts())[id]
		return exists && account.CanCheck && account.Revision == accountRevision && validate() == nil
	}
	if ctx.Err() != nil || !current() {
		return ErrSuperseded
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	row, hadRow := s.rows[id]
	previous := row
	previous.Snapshot = cloneSnapshot(row.Snapshot)
	row.SchemaVersion, row.Snapshot.ID = 1, id
	row.Snapshot = cloneSnapshot(row.Snapshot)
	row.Snapshot.ProbeHistory = normalizeProbeHistory(append(row.Snapshot.ProbeHistory, item))
	s.mu.Unlock()
	rollback := func() error {
		// writeGate still excludes every other quality writer. A missing
		// original row must remain missing instead of acquiring business state.
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if !hadRow {
			return privatefile.Remove(rollbackCtx, s.disk.Path(snapshotFile(id)))
		}
		return s.persist(rollbackCtx, snapshotFile(id), previous)
	}
	if !current() {
		return ErrSuperseded
	}
	if err := s.persist(ctx, snapshotFile(id), row); err != nil {
		_ = rollback()
		return ErrCachePersistence
	}
	if !current() {
		if err := rollback(); err != nil {
			return ErrCachePersistence
		}
		return ErrSuperseded
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := s.rows[id]
	latest.SchemaVersion, latest.Snapshot.ID = 1, id
	latest.Snapshot.ProbeHistory = normalizeProbeHistory(row.Snapshot.ProbeHistory)
	s.rows[id] = latest
	s.version++
	return nil
}
