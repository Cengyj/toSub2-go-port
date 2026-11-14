package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// All recycle-bin records, outcomes and logs are synthetic and memory-only.
// The fixture server holds its mutex while calling these methods.
type trashFixtureEntry struct {
	id, email, state, lastError string
	deletedAt                   time.Time
	purgeStartedAt              *time.Time
	jobs                        []map[string]any
}
type trashFixture struct {
	entries                  map[string]*trashFixtureEntry
	retentionDays            int
	revision                 uint64
	sequence                 int
	outcome, settingsOutcome string
	readError, logWarning    bool
	persistenceUnconfirmed   bool
	requests                 []map[string]any
	lastRunAt                *time.Time
	dataset                  string
}

func newTrashFixture() *trashFixture {
	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour)
	return &trashFixture{entries: map[string]*trashFixtureEntry{"synthetic-trash-1": {id: "synthetic-trash-1", email: "archived.synthetic@example.invalid", state: "trashed", deletedAt: old, jobs: []map[string]any{{"id": "archived-job-1", "email": "archived.synthetic@example.invalid", "status": "completed", "operation": "relogin", "lastOperationType": "relogin", "createdAt": old.Add(-time.Hour).Format(time.RFC3339), "lastOperationAt": old.Format(time.RFC3339), "hasLogs": true, "canDownload": true}, {"id": "archived-job-2", "email": "archived.synthetic@example.invalid", "status": "failed", "operation": "refresh", "createdAt": old.Format(time.RFC3339), "hasLogs": true}}}}, revision: 1, sequence: 1, outcome: "success", settingsOutcome: "success", requests: []map[string]any{}, dataset: "default"}
}

// The large dataset is opt-in so existing fixture captures keep their defaults.
// All timestamps are valid production DTO values; a zero deletedAt would be
// rejected by the real recovery loader and is deliberately never fabricated.
func (f *trashFixture) seedLarge() {
	now := time.Now().UTC()
	f.dataset, f.retentionDays = "large", 7
	for i := 2; i <= 84; i++ {
		id := fmt.Sprintf("synthetic-trash-%d", i)
		email := fmt.Sprintf("archived-%03d@example.invalid", i)
		if i%7 == 0 {
			email = fmt.Sprintf("archive.layout.long-local-name.case-%03d+recovery@department-with-a-long-synthetic-name.example.invalid", i)
		}
		deleted := now.Add(-time.Duration(i%7)*24*time.Hour - time.Duration(i%10)*time.Hour)
		if i%19 == 0 {
			deleted = now.Add(-7*24*time.Hour + 45*time.Minute)
		}
		entry := &trashFixtureEntry{id: id, email: email, state: "trashed", deletedAt: deleted}
		if i%5 == 0 {
			entry.state = "purging"
			started := now.Add(-15 * time.Minute)
			if started.Before(deleted) {
				started = deleted
			}
			entry.purgeStartedAt = &started
			entry.lastError = strings.Repeat("合成清理错误：安全凭据服务暂不可用；已清理步骤不会自动恢复，剩余资料仍保留，请修复本地存储后重试完成永久清理。", 3)
		}
		history := i%4 + 1
		if i%13 == 0 {
			history = 9
		}
		for n := 0; n < history; n++ {
			operation := []string{"account_import", "relogin", "refresh", "reset_2fa"}[(i+n)%4]
			entry.jobs = append(entry.jobs, map[string]any{"id": fmt.Sprintf("archived-%03d-job-%02d", i, n+1), "email": email, "status": []string{"imported", "completed", "failed", "canceled"}[(i+n)%4], "operation": operation, "lastOperationType": operation, "createdAt": deleted.Add(-time.Duration(history-n) * 24 * time.Hour).Format(time.RFC3339), "updatedAt": deleted.Add(-time.Duration(history-n) * time.Minute).Format(time.RFC3339), "hasLogs": i%4 != 0 && n%2 == 0})
		}
		f.entries[id] = entry
	}
	f.sequence = 84
}

func (f *trashFixture) configure(q url.Values) {
	if dataset := q.Get("trashDataset"); dataset == "default" || dataset == "large" {
		*f = *newTrashFixture()
		if dataset == "large" {
			f.seedLarge()
		}
	}
	if value := q.Get("trashRetentionDays"); value != "" {
		if days, err := strconv.Atoi(value); err == nil && days >= 0 && days <= 3650 {
			f.retentionDays = days
			f.revision++
		}
	}
	for key, target := range map[string]*string{"trashOutcome": &f.outcome, "trashSettings": &f.settingsOutcome} {
		switch value := q.Get(key); value {
		case "success", "failed", "partial", "unknown", "conflict":
			*target = value
		}
	}
	if q.Has("trashReadError") {
		f.readError = q.Get("trashReadError") == "true"
	}
	if q.Has("trashLogWarning") {
		f.logWarning = q.Get("trashLogWarning") == "true"
	}
}
func (f *trashFixture) settings() map[string]any {
	settings := map[string]any{"schemaVersion": 1, "revision": f.revision, "retentionDays": f.retentionDays}
	if f.persistenceUnconfirmed {
		settings["persistenceWarning"] = "合成策略持久化未确认，自动清理暂停，需明确重新保存。"
	}
	return settings
}
func (f *trashFixture) state() map[string]any {
	return map[string]any{"settings": f.settings(), "entries": len(f.entries), "dataset": f.dataset, "outcome": f.outcome, "settingsOutcome": f.settingsOutcome, "readError": f.readError, "requests": f.requests}
}
func (f *trashFixture) cleanExpired() {
	if f.retentionDays == 0 || f.persistenceUnconfirmed {
		return
	}
	now := time.Now().UTC()
	f.lastRunAt = &now
	for id, entry := range f.entries {
		if !now.Before(entry.deletedAt.Add(time.Duration(f.retentionDays) * 24 * time.Hour)) {
			delete(f.entries, id)
		}
	}
}
func (f *trashFixture) list() map[string]any {
	f.cleanExpired()
	entries := []map[string]any{}
	ids := make([]string, 0, len(f.entries))
	for id := range f.entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := f.entries[ids[i]], f.entries[ids[j]]
		if left.deletedAt.Equal(right.deletedAt) {
			return left.id < right.id
		}
		return left.deletedAt.After(right.deletedAt)
	})
	for _, id := range ids {
		entry := f.entries[id]
		history := make([]map[string]any, 0, len(entry.jobs))
		for _, job := range entry.jobs {
			operationType, _ := job["lastOperationType"].(string)
			updated := job["updatedAt"]
			if updated == nil {
				updated = job["createdAt"]
			}
			history = append(history, map[string]any{"id": job["id"], "status": job["status"], "operation": job["operation"], "lastOperationType": operationType, "createdAt": job["createdAt"], "updatedAt": updated, "hasLogs": job["hasLogs"] == true})
		}
		projected := map[string]any{"id": entry.id, "email": entry.email, "deletedAt": entry.deletedAt, "state": entry.state, "recordCount": len(entry.jobs), "jobs": history, "canRestore": entry.state == "trashed"}
		if entry.purgeStartedAt != nil {
			projected["purgeStartedAt"] = entry.purgeStartedAt
		}
		if entry.lastError != "" {
			projected["lastError"] = entry.lastError
		}
		if f.retentionDays > 0 {
			projected["expiresAt"] = entry.deletedAt.Add(time.Duration(f.retentionDays) * 24 * time.Hour)
		}
		entries = append(entries, projected)
	}
	return map[string]any{"entries": entries, "settings": f.settings(), "status": map[string]any{"running": false, "lastRunAt": f.lastRunAt}}
}
func cloneTrashFixtureJob(job map[string]any) map[string]any {
	data, _ := json.Marshal(job)
	var copy map[string]any
	_ = json.Unmarshal(data, &copy)
	return copy
}
func (f *trashFixture) writeLogs(w http.ResponseWriter, r *http.Request, id string, write func(http.ResponseWriter, int, any)) {
	const count = 620
	before := count
	if cursor := r.URL.Query().Get("before"); cursor != "" {
		value, err := strconv.Atoi(strings.TrimPrefix(cursor, "synthetic-before:"))
		if err != nil || !strings.HasPrefix(cursor, "synthetic-before:") || value < 0 || value > count {
			write(w, 400, map[string]any{"error": "合成日志分页标记无效"})
			return
		}
		before = value
	}
	start := max(0, before-500)
	entries := []map[string]any{}
	lines := []string{}
	for index := start; index < before; index++ {
		at := time.Date(2026, 10, 12, 8, 0, index, 0, time.UTC)
		operationAt := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
		operation := "relogin"
		if index >= 310 {
			operationAt = operationAt.Add(310 * time.Second)
			operation = "refresh"
		}
		message := fmt.Sprintf("Synthetic retained log %d for %s", index+1, id)
		entries = append(entries, map[string]any{"at": at.Format(time.RFC3339), "operationAt": operationAt.Format(time.RFC3339), "operation": operation, "level": "info", "message": message})
		lines = append(lines, at.Format(time.RFC3339)+" [info] "+message)
	}
	next := ""
	if start > 0 {
		next = fmt.Sprintf("synthetic-before:%d", start)
	}
	result := map[string]any{"id": id, "logs": strings.Join(lines, "\n"), "entries": entries, "hasMore": start > 0, "nextBefore": next}
	if f.logWarning {
		result["warning"] = "合成提示：较早版本已截断的日志无法恢复；当前展示保留记录。"
	}
	write(w, 200, result)
}
func (f *trashFixture) handle(w http.ResponseWriter, r *http.Request, jobs *[]map[string]any, write func(http.ResponseWriter, int, any)) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/account-trash") {
		return false
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/account-trash")
	if r.Method == http.MethodGet {
		if f.readError {
			write(w, 503, map[string]any{"error": "合成回收站暂不可读"})
			return true
		}
		if path == "" {
			write(w, 200, f.list())
			return true
		}
		if path == "/settings" {
			write(w, 200, f.settings())
			return true
		}
		if strings.HasSuffix(path, "/logs") {
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/logs")
			entry := f.entries[id]
			if entry != nil {
				for _, job := range entry.jobs {
					if job["id"] == r.URL.Query().Get("jobId") {
						if job["hasLogs"] != true {
							write(w, 200, map[string]any{"id": job["id"], "logs": "", "entries": []any{}, "hasMore": false})
							return true
						}
						f.writeLogs(w, r, job["id"].(string), write)
						return true
					}
				}
			}
			write(w, 404, map[string]any{"error": "合成归档日志不存在"})
			return true
		}
	}
	if path == "/settings" && r.Method == http.MethodPut {
		var body struct {
			SchemaVersion, RetentionDays int
			Revision                     uint64
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.SchemaVersion != 1 || body.RetentionDays < 0 || body.RetentionDays > 3650 {
			write(w, 400, map[string]any{"error": "保留天数必须为0–3650整数", "outcome": "failed"})
			return true
		}
		f.requests = append(f.requests, map[string]any{"action": "settings", "retentionDays": body.RetentionDays})
		if body.Revision != f.revision || f.settingsOutcome == "conflict" {
			write(w, 409, map[string]any{"settings": f.settings(), "outcome": "failed", "error": "合成配置版本冲突"})
			return true
		}
		if f.settingsOutcome == "failed" {
			write(w, 503, map[string]any{"settings": f.settings(), "outcome": "failed", "error": "合成配置持久化失败"})
			return true
		}
		f.retentionDays = body.RetentionDays
		f.revision++
		f.persistenceUnconfirmed = f.settingsOutcome == "unknown"
		f.cleanExpired()
		if f.settingsOutcome == "unknown" {
			write(w, 503, map[string]any{"settings": f.settings(), "outcome": "unknown", "uncertain": true, "error": "合成保存结果待核实"})
			return true
		}
		write(w, 200, f.settings())
		return true
	}
	action := strings.TrimPrefix(path, "/")
	if r.Method != http.MethodPost || action != "move" && action != "restore" && action != "purge" {
		write(w, 404, map[string]any{"error": "合成回收站路由不存在"})
		return true
	}
	var body struct {
		IDs              []string
		ConfirmPermanent bool
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.IDs) == 0 || len(body.IDs) > 1000 || action == "purge" && !body.ConfirmPermanent {
		write(w, 400, map[string]any{"error": "合成回收站请求无效"})
		return true
	}
	ids := make([]string, 0, len(body.IDs))
	seen := map[string]bool{}
	for _, id := range body.IDs {
		if strings.TrimSpace(id) == "" {
			write(w, 400, map[string]any{"error": "合成账号标识不能为空"})
			return true
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	f.requests = append(f.requests, map[string]any{"action": action, "ids": body.IDs, "confirmPermanent": body.ConfirmPermanent})
	key := map[string]string{"move": "moved", "restore": "restored", "purge": "purged"}[action]
	// The real API validates the whole selection before changing a restore or
	// purge. In particular a mixed selection cannot partly restore a purging ID.
	if action != "move" {
		for _, id := range ids {
			entry := f.entries[id]
			if entry == nil {
				write(w, 404, map[string]any{"error": "合成回收记录不存在，请刷新核对"})
				return true
			}
			if action == "restore" {
				failure := ""
				if entry.state != "trashed" {
					failure = "合成永久清理已开始，不能恢复；请重试完成清理"
				}
				for _, job := range *jobs {
					if email, _ := job["email"].(string); strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(entry.email)) {
						failure = "活动列表已有同邮箱账号，请核对冲突"
					}
				}
				if failure != "" {
					write(w, 409, map[string]any{key: 0, "affectedIds": []string{}, "outcome": "failed", "error": failure})
					return true
				}
			}
		}
	}
	if action == "purge" && f.outcome != "failed" && f.outcome != "conflict" {
		// Durable purging applies to the whole selection before cleanup starts.
		// A partial result can remove one item and retain irreversible peers.
		for _, id := range ids {
			entry := f.entries[id]
			entry.state, entry.lastError = "purging", ""
			if entry.purgeStartedAt == nil {
				started := time.Now().UTC()
				entry.purgeStartedAt = &started
			}
		}
	}
	count := 0
	affected := []string{}
	failure := ""
	for _, id := range ids {
		partialFailure := f.outcome == "partial" && (count > 0 || action == "restore" || action == "purge" && len(ids) == 1)
		if f.outcome == "failed" || f.outcome == "conflict" || partialFailure {
			failure = "合成持久化或身份冲突，记录仍保留"
			if entry := f.entries[id]; action == "purge" && entry != nil && entry.state == "purging" {
				entry.lastError = "合成永久清理已开始但本地凭据清理失败，剩余资料保留，不能恢复；修复后可重试。"
			}
			break
		}
		if action == "move" {
			email := ""
			for _, job := range *jobs {
				if job["id"] == id {
					email, _ = job["email"].(string)
					break
				}
			}
			if email == "" {
				failure = "合成活动账号不存在"
				break
			}
			f.sequence++
			entry := &trashFixtureEntry{id: fmt.Sprintf("synthetic-trash-%d", f.sequence), email: email, state: "trashed", deletedAt: time.Now().UTC()}
			remaining := []map[string]any{}
			for _, job := range *jobs {
				if strings.EqualFold(job["email"].(string), email) {
					copy := cloneTrashFixtureJob(job)
					copy["hasLogs"] = true
					entry.jobs = append(entry.jobs, copy)
					affected = append(affected, job["id"].(string))
				} else {
					remaining = append(remaining, job)
				}
			}
			*jobs = remaining
			f.entries[entry.id] = entry
			count++
			continue
		}
		entry := f.entries[id]
		if entry == nil {
			failure = "合成回收记录不存在"
			break
		}
		if action == "restore" {
			if entry.state != "trashed" {
				failure = "合成永久清理已开始，暂不可恢复"
				break
			}
			conflict := false
			for _, job := range *jobs {
				if strings.EqualFold(job["email"].(string), entry.email) {
					conflict = true
				}
			}
			if conflict {
				failure = "活动列表已有同邮箱账号，请核对冲突"
				break
			}
			for _, job := range entry.jobs {
				*jobs = append(*jobs, cloneTrashFixtureJob(job))
			}
		}
		for _, job := range entry.jobs {
			affected = append(affected, job["id"].(string))
		}
		delete(f.entries, id)
		count++
	}
	sort.Strings(affected)
	purgePending := false
	if action == "purge" {
		for _, id := range ids {
			if entry := f.entries[id]; entry != nil && entry.state == "purging" {
				purgePending = true
			}
		}
	}
	outcome, status := "completed", 200
	if failure != "" {
		outcome, status = "failed", 409
		if count > 0 || purgePending {
			outcome = "partial"
		}
	}
	if f.outcome == "unknown" {
		outcome, status, failure = "unknown", 503, "合成回执丢失，请读取列表核实"
	}
	response := map[string]any{key: count, "affectedIds": affected, "outcome": outcome}
	if action == "move" {
		response["deleted"] = count
	}
	if failure != "" {
		response["error"] = failure
	}
	if purgePending {
		response["purgePending"] = true
	}
	if outcome == "unknown" {
		response["uncertain"] = true
	}
	write(w, status, response)
	return true
}
