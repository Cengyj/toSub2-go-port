package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/store"
)

// The aggregate keeps a short UI summary. Immutable private records retain
// history without growing that bounded startup document. Retrying is idempotent.
type archivedLogRecord struct {
	Entry      *LogEntry `json:"entry,omitempty"`
	Text       string    `json:"text"`
	Occurrence int       `json:"occurrence,omitempty"`
}

// logPageFields deliberately has no LogEntry.UnmarshalJSON method: this is an
// API projection, not the persisted event or its content-addressed archive.
type logPageFields LogEntry

type LogPageEntry struct {
	logPageFields
	// LegacyText says Message is the entire original text record. Clients can
	// pair it with Logs once without guessing from the message's contents.
	LegacyText bool `json:"legacyText,omitempty"`
}

type LogPage struct {
	ID         string         `json:"id"`
	Logs       string         `json:"logs"`
	Entries    []LogPageEntry `json:"entries"`
	HasMore    bool           `json:"hasMore"`
	NextBefore string         `json:"nextBefore,omitempty"`
	Warning    string         `json:"warning,omitempty"`
}

var logRecordName = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[a-f0-9]{64}\.json$`)

// Persist only records about to leave the bounded aggregate summary. The
// current tail remains in the existing atomic metadata transaction.
func (m *Manager) archiveOverflowLogsLocked(j *Job) error {
	if len(j.Logs) <= 200 && len(j.LogEntries) <= 200 {
		return nil
	}
	// Legacy plain text lacks stable event timestamps/identity. Capture its
	// whole retained window before the first trim so repeated lines keep
	// their original multiplicity as the window subsequently shrinks.
	structured := map[string]int{}
	seen := map[string]bool{}
	for _, e := range j.LogEntries {
		text := fmt.Sprintf("%s [%s] %s", e.At.Format(time.RFC3339), e.Level, e.Message)
		entry := e
		key := logName(archivedLogRecord{Entry: &entry, Text: text})
		if seen[key] {
			return m.archiveRetainedLogsLocked(j)
		}
		seen[key] = true
		structured[text]++
	}
	for _, line := range j.Logs {
		if structured[line] == 0 {
			return m.archiveRetainedLogsLocked(j)
		}
		structured[line]--
	}
	snapshot := *j
	snapshot.Logs = nil
	snapshot.LogEntries = nil
	if len(j.Logs) > 200 {
		snapshot.Logs = j.Logs[:len(j.Logs)-200]
	}
	if len(j.LogEntries) > 200 {
		snapshot.LogEntries = j.LogEntries[:len(j.LogEntries)-200]
	}
	return m.archiveRetainedLogsLocked(&snapshot)
}

func logRecords(j *Job) map[string]archivedLogRecord {
	j = SanitizeJobForPublic(j)
	out := map[string]archivedLogRecord{}
	structured := map[string]int{}
	occurrences := map[string]int{}
	for _, e := range j.LogEntries {
		entry := e
		text := fmt.Sprintf("%s [%s] %s", e.At.Format(time.RFC3339), e.Level, e.Message)
		rec := archivedLogRecord{Entry: &entry, Text: text}
		key := logName(rec)
		rec.Occurrence = occurrences[key]
		occurrences[key]++
		out[logName(rec)] = rec
		structured[text]++
	}
	for _, line := range j.Logs {
		if structured[line] > 0 {
			structured[line]--
			continue
		}
		rec := archivedLogRecord{Text: line}
		key := logName(rec)
		rec.Occurrence = occurrences[key]
		occurrences[key]++
		out[logName(rec)] = rec
	}
	return out
}
func logName(record archivedLogRecord) string {
	at := time.Time{}
	if record.Entry != nil {
		at = record.Entry.At
	} else if prefix, _, ok := strings.Cut(record.Text, " "); ok {
		at, _ = time.Parse(time.RFC3339, prefix)
	}
	data, _ := json.Marshal(record)
	sum := sha256.Sum256(data)
	return at.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(sum[:]) + ".json"
}
func (m *Manager) logDirectory(id string, create bool) (string, error) {
	if m.store == nil {
		return "", nil
	}
	if !safeID(id) {
		return "", errors.New("invalid log identity")
	}
	root := m.store.Root
	for _, p := range []string{root, filepath.Join(root, id), filepath.Join(root, id, "operation-logs")} {
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) && create {
			if err = privatefile.EnsureDir(p); err != nil {
				return "", err
			}
			info, err = os.Lstat(p)
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("unsafe log directory")
		}
	}
	return filepath.Join(root, id, "operation-logs"), nil
}

// Called under the manager lock before trimming or archiving an account.
// Failure leaves the untrimmed summary available and blocks soft delete.
func (m *Manager) archiveRetainedLogsLocked(j *Job, contexts ...context.Context) error {
	if m.store == nil || len(j.Logs)+len(j.LogEntries) == 0 {
		return nil
	}
	ctx := m.rootCtx
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := m.logDirectory(j.ID, true)
	if err != nil {
		return err
	}
	disk := &store.Store{Root: dir}
	records := logRecords(j)
	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err == nil {
			if !info.Mode().IsRegular() {
				return errors.New("unsafe log record")
			}
			if _, err := readLogRecord(ctx, dir, name); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = disk.WriteJSON(ctx, name, records[name]); err != nil {
			return err
		}
	}
	// Reconfirm earlier uncertain writes and newly created directory entries.
	if runtime.GOOS != "windows" {
		for _, path := range []string{dir, filepath.Dir(dir), m.store.Root} {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			err = f.Sync()
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	return ctx.Err()
}
func readLogRecord(ctx context.Context, dir, name string) (archivedLogRecord, error) {
	var record archivedLogRecord
	if err := ctx.Err(); err != nil {
		return record, err
	}
	path := filepath.Join(dir, name)
	before, err := os.Lstat(path)
	if err != nil {
		return record, err
	}
	if !before.Mode().IsRegular() {
		return record, errors.New("unsafe log record")
	}
	f, err := os.Open(path)
	if err != nil {
		return record, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return record, err
	}
	if !os.SameFile(before, after) || after.Size() > 32<<20 {
		return record, errors.New("invalid log record")
	}
	if err = privatefile.Verify(f); err != nil {
		return record, err
	}
	d := json.NewDecoder(io.LimitReader(f, (32<<20)+1))
	if err = d.Decode(&record); err != nil {
		return record, err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return record, errors.New("invalid log record suffix")
	}
	if logName(record) != name {
		return record, errors.New("log record integrity check failed")
	}
	return record, ctx.Err()
}

// ReadLogPage is read-only and bounded to 500 records per page. The opaque
// content-address cursor keeps older pages stable as newer events arrive.
func (m *Manager) ReadLogPage(ctx context.Context, j *Job, before string) (LogPage, error) {
	page := LogPage{ID: j.ID, Entries: []LogPageEntry{}}
	if before != "" && !logRecordName.MatchString(before) {
		return page, errors.New("invalid log cursor")
	}
	records := logRecords(j)
	dir, err := m.logDirectory(j.ID, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return page, err
	}
	names := map[string]bool{}
	for name := range records {
		names[name] = true
	}
	if err == nil && dir != "" {
		files, e := os.ReadDir(dir)
		if e != nil {
			return page, e
		}
		for _, f := range files {
			if logRecordName.MatchString(f.Name()) {
				if !f.Type().IsRegular() {
					return page, errors.New("unsafe log record")
				}
				names[f.Name()] = true
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		if before == "" || name < before {
			ordered = append(ordered, name)
		}
	}
	sort.Strings(ordered)
	if len(ordered) > 500 {
		page.HasMore = true
		ordered = ordered[len(ordered)-500:]
	}
	lines := make([]string, 0, len(ordered))
	for _, name := range ordered {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		rec, ok := records[name]
		if !ok {
			var e error
			rec, e = readLogRecord(ctx, dir, name)
			if e != nil {
				return page, e
			}
		}
		snapshot := *j
		snapshot.Logs = []string{rec.Text}
		snapshot.LogEntries = nil
		if rec.Entry != nil {
			entry := *rec.Entry
			// Older formatters treated a local commit as a factor match or a
			// missing logout result as unsent. Render only the public copy.
			if d := entry.Diagnostic; d != nil && d.Terminal && d.Commit == "committed" && (d.Reason == "RESET_COMPLETE_LOGOUT_UNCONFIRMED" || d.Logout == "not_performed") {
				if detail := d.Safe(); detail != nil {
					entry.Message, entry.Level = detail.Message(), detail.Level()
					snapshot.Logs[0] = fmt.Sprintf("%s [%s] %s", entry.At.Format(time.RFC3339), entry.Level, entry.Message)
				}
			}
			snapshot.LogEntries = []LogEntry{entry}
		} else {
			// Mixed older text and structured history must both remain visible
			// to clients that prefer entries over the raw text fallback.
			legacy := LogEntry{Level: "info", Message: rec.Text}
			if prefix, _, ok := strings.Cut(rec.Text, " "); ok {
				legacy.At, _ = time.Parse(time.RFC3339, prefix)
			}
			snapshot.LogEntries = []LogEntry{legacy}
		}
		safe := SanitizeJobForPublic(&snapshot)
		lines = append(lines, safe.Logs...)
		for _, entry := range safe.LogEntries {
			page.Entries = append(page.Entries, LogPageEntry{logPageFields: logPageFields(entry), LegacyText: rec.Entry == nil})
		}
	}
	page.Logs = strings.Join(lines, "\n")
	if page.HasMore && len(ordered) > 0 {
		page.NextBefore = ordered[0]
	}
	for _, warning := range credentialWarningLines(j.LastError) {
		if warning == operationLogArchiveWarning {
			// A readable aggregate does not confirm its pending archive commit.
			// Expose only this fixed log warning, never unrelated storage errors.
			page.Warning = operationLogArchiveWarning
			break
		}
	}
	return page, nil
}
