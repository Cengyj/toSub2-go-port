package recyclebin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"tosub2-go-port/internal/jobs"
)

func TestRetentionSkipsConfirmedMissingSnapshotEntry(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprint(removed), func(t *testing.T) {
			r := &fakeRepository{entries: []*jobs.TrashEntry{{ID: "moved", State: jobs.TrashStatePurging}, {ID: "next", State: jobs.TrashStatePurging}}}
			r.purge = func(_ context.Context, id string) (int, error) {
				if id == "moved" {
					if removed {
						r.mu.Lock()
						r.entries = r.entries[1:]
						r.mu.Unlock()
					}
					return 0, jobs.ErrTrashNotFound
				}
				return 1, nil
			}
			s := newService(t, r)
			err := s.Sweep(t.Context(), time.Now())
			status := s.Status()
			if removed {
				if err != nil || len(r.calls) != 2 || status.LastPurged != 1 || status.LastError != "" {
					t.Fatalf("resolved snapshot race stopped later cleanup: err=%v calls=%v status=%+v", err, r.calls, status)
				}
			} else if !errors.Is(err, jobs.ErrTrashNotFound) || len(r.calls) != 1 || status.LastError == "" {
				t.Fatalf("unconfirmed missing entry was silently skipped: err=%v calls=%v status=%+v", err, r.calls, status)
			}
		})
	}
}

func TestRetentionBusyEntryDoesNotBlockLaterCleanupAndResolvedStatusClears(t *testing.T) {
	r := &fakeRepository{entries: []*jobs.TrashEntry{{ID: "busy", State: jobs.TrashStatePurging}, {ID: "next", State: jobs.TrashStatePurging}}}
	r.purge = func(_ context.Context, id string) (int, error) {
		if id == "busy" {
			return 0, jobs.TrashCleanupFailure(jobs.ErrTrashLocalCleanup, jobs.ErrTrashBusy)
		}
		return 1, nil
	}
	s := newService(t, r)
	now := time.Now().UTC()
	if err := s.Sweep(t.Context(), now); !errors.Is(err, jobs.ErrTrashBusy) || len(r.calls) != 2 {
		t.Fatalf("busy record blocked independent work: err=%v calls=%v", err, r.calls)
	}
	before := s.Status()
	if before.LastPurged != 1 || before.LastRunAt == nil || !before.LastRunAt.Equal(now) || !strings.Contains(before.LastError, "其他操作") || strings.Contains(before.LastError, "存储") {
		t.Fatalf("busy record was described as a storage failure: %+v", before)
	}
	r.mu.Lock()
	r.entries = r.entries[1:]
	r.mu.Unlock()
	after := s.Status()
	if after.LastError != "" || after.LastPurged != before.LastPurged || !after.LastRunAt.Equal(*before.LastRunAt) {
		t.Fatalf("resolved record left a stale warning or erased run history: %+v", after)
	}
	if s.status.LastError == "" {
		t.Fatal("status read overwrote the stored historical result")
	}
}

func TestRetentionBusyDeferralKeepsBatchLimit(t *testing.T) {
	r := &fakeRepository{purge: func(context.Context, string) (int, error) { return 0, jobs.ErrTrashBusy }}
	for i := 0; i < 25; i++ {
		r.entries = append(r.entries, &jobs.TrashEntry{ID: fmt.Sprint(i), State: jobs.TrashStatePurging})
	}
	s := newService(t, r)
	if err := s.Sweep(t.Context(), time.Now()); !errors.Is(err, jobs.ErrTrashBusy) || len(r.calls) != 20 || s.Status().LastPurged != 0 {
		t.Fatalf("deferred work escaped the batch limit: err=%v calls=%d", err, len(r.calls))
	}
}

func TestRetentionStatusUsesSafeFailureCategoryAndOnlyReconcilesRecordFailures(t *testing.T) {
	private := errors.New("synthetic private /account/secret and credential response")
	for _, tc := range []struct {
		name     string
		err      error
		contains string
		resolved bool
	}{
		{"deadline", jobs.TrashCleanupFailure(jobs.ErrTrashCredentialCleanup, context.DeadlineExceeded), "超时", false},
		{"cancelled", context.Canceled, "中断", false},
		{"policy-uncertain", ErrUncertain, "设置保存结果待核实", false},
		{"persistence-uncertain", errors.Join(jobs.ErrTrashUncertain, private), "持久性尚未确认", false},
		{"persistence", errors.Join(jobs.ErrTrashPersistence, private), "变更保存失败", false},
		{"inspection", jobs.TrashCleanupFailure(jobs.ErrTrashInspection, private), "历史文件检查", false},
		{"credentials", jobs.TrashCleanupFailure(jobs.ErrTrashCredentialCleanup, private), "安全凭据清理", true},
		{"directories", jobs.TrashCleanupFailure(jobs.ErrTrashDirectoryCleanup, private), "本地文件删除", true},
		{"insights", jobs.TrashCleanupFailure(jobs.ErrTrashInsightCleanup, private), "用量缓存清理", true},
		{"quality", jobs.TrashCleanupFailure(jobs.ErrTrashQualityCleanup, private), "模型检测缓存清理", true},
		{"sms", jobs.TrashCleanupFailure(jobs.ErrTrashSMSCleanup, private), "短信账号数据清理", true},
		{"conflict", errors.Join(jobs.ErrTrashConflict, private), "冲突", true},
		{"unknown", private, "查看记录详情", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRepository{entries: []*jobs.TrashEntry{{ID: "failed", State: jobs.TrashStatePurging}, {ID: "later", State: jobs.TrashStatePurging}}, purge: func(context.Context, string) (int, error) { return 0, tc.err }}
			s := newService(t, r)
			if err := s.Sweep(t.Context(), time.Now()); !errors.Is(err, tc.err) || len(r.calls) != 1 {
				t.Fatalf("terminal failure continued cleanup: err=%v calls=%v", err, r.calls)
			}
			before := s.Status()
			if !strings.Contains(before.LastError, tc.contains) || strings.Contains(before.LastError, "secret") || strings.Contains(before.LastError, "credential response") {
				t.Fatalf("wrong or unsafe failure category: %+v", before)
			}
			r.mu.Lock()
			r.entries = nil
			r.mu.Unlock()
			after := s.Status()
			if (after.LastError == "") != tc.resolved || after.LastRunAt == nil || !after.LastRunAt.Equal(*before.LastRunAt) {
				t.Fatalf("record reconciliation changed a system failure or run history: %+v", after)
			}
			if err := s.Sweep(t.Context(), time.Now()); err != nil || s.Status().LastError != "" {
				t.Fatal("successful empty sweep retained the previous failure", err)
			}
		})
	}
}

func TestRetentionBusyCannotHidePersistenceOrContextFailure(t *testing.T) {
	for _, failure := range []error{jobs.ErrTrashPersistence, jobs.ErrTrashUncertain, jobs.ErrTrashInspection, context.DeadlineExceeded, context.Canceled} {
		r := &fakeRepository{entries: []*jobs.TrashEntry{{ID: "failed", State: jobs.TrashStatePurging}, {ID: "later", State: jobs.TrashStatePurging}}, purge: func(context.Context, string) (int, error) { return 0, errors.Join(jobs.ErrTrashBusy, failure) }}
		s := newService(t, r)
		if err := s.Sweep(t.Context(), time.Now()); !errors.Is(err, failure) || len(r.calls) != 1 || strings.Contains(s.Status().LastError, "其他操作") {
			t.Fatalf("busy classification swallowed terminal failure: err=%v calls=%v status=%+v", err, r.calls, s.Status())
		}
	}
}
