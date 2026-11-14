package console

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
)

var errSMSTrashBusy = errors.New("接码取号或订单状态更新仍在进行，请等待结束后重试回收")
var errSMSTrashPersistence = errors.New("接码恢复记录未能安全保存，账号尚未移入回收站")
var errSMSTrashPurge = errors.New("接码私有记录未完全清理，请重试完成永久删除")

// pauseTrashSMS is called with the manager's email mutation gate held, but
// without its mutex. Hold the existing persistence-before-SMS lock order so
// neither completion nor a late save can erase the archived recovery state.
func (s *Server) pauseTrashSMS(parent context.Context, selected []*jobs.Job) (err error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return errors.Join(errSMSTrashPersistence, err)
	}
	smsPersistenceMu.Lock()
	defer smsPersistenceMu.Unlock()
	if err := ctx.Err(); err != nil {
		return errors.Join(errSMSTrashPersistence, err)
	}
	targets := make(map[string]*jobs.Job, len(selected))
	for _, job := range selected {
		if job == nil || job.ID == "" {
			return errSMSTrashPersistence
		}
		targets[job.ID] = job
	}
	ids := make([]string, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	s.smsMu.Lock()
	if s.smsTrashing == nil {
		s.smsTrashing = map[string]bool{}
	}
	for id := range targets {
		if s.smsAcquiring[id] || s.smsCompleting[id] || s.smsTrashing[id] {
			s.smsMu.Unlock()
			return errSMSTrashBusy
		}
	}
	leases := make(map[string]*smsLease, len(targets))
	states := make(map[string]privateSMSState, len(targets))
	for id, job := range targets {
		s.smsTrashing[id] = true
		if l := s.smsLeases[id]; l != nil {
			state, snapshotErr := s.snapshotSMSLocked(id, l)
			if snapshotErr != nil {
				for key := range targets {
					delete(s.smsTrashing, key)
				}
				s.smsMu.Unlock()
				return errSMSTrashPersistence
			}
			// Use the manager's captured identity even if public lookup becomes
			// hidden during deletion. The private record must stay restorable.
			state.Email, state.ServiceLabel = job.Email, job.SMSServiceLabel
			states[id], leases[id] = state, l
		}
	}
	s.smsMu.Unlock()
	defer func() {
		if err != nil {
			s.resumeTrashSMS(ids)
		}
	}()
	for id, job := range targets {
		state, exists := states[id]
		if !exists {
			if s.monitorVault == nil {
				if job.SMSProviderID == "" {
					continue
				}
				return errSMSTrashPersistence
			}
			stored, loadErr := credentials.LoadContext(ctx, s.monitorVault, smsCredentialID(id))
			if errors.Is(loadErr, os.ErrNotExist) {
				continue
			}
			if loadErr != nil || json.Unmarshal([]byte(stored.Password), &state) != nil || state.Version != 1 || state.JobID != id || !strings.EqualFold(strings.TrimSpace(state.Email), strings.TrimSpace(job.Email)) {
				return errors.Join(errSMSTrashPersistence, ctx.Err())
			}
		}
		state.TrashPaused = true
		encoded, marshalErr := json.Marshal(state)
		if marshalErr != nil || s.monitorVault == nil {
			return errSMSTrashPersistence
		}
		if saveErr := credentials.SaveContext(ctx, s.monitorVault, smsCredentialID(id), credentials.Credentials{Password: string(encoded)}); saveErr != nil {
			return errors.Join(errSMSTrashPersistence, ctx.Err())
		}
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(errSMSTrashPersistence, err)
	}
	// All private records are durable before any runner loses ownership.
	s.smsMu.Lock()
	for id, l := range leases {
		if s.smsLeases[id] == l {
			delete(s.smsLeases, id)
			l.generation++
			if l.pollCancel != nil {
				l.pollCancel()
			}
			l.cancel()
		}
	}
	s.smsMu.Unlock()
	return nil
}

// resumeTrashSMS releases only the admission fence. Call after the manager has
// finished a trash/restore attempt. It never installs an archived SMS lease.
func (s *Server) resumeTrashSMS(ids []string) {
	s.smsMu.Lock()
	defer s.smsMu.Unlock()
	for _, id := range ids {
		delete(s.smsTrashing, id)
	}
}

// purgeTrashSMS is a local, retryable cleanup after durable purging. Global
// custom-number-pool records are shared and deliberately remain untouched.
func (s *Server) purgeTrashSMS(parent context.Context, entry *jobs.TrashEntry) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	if entry == nil || entry.State != jobs.TrashStatePurging {
		return errSMSTrashPurge
	}
	smsPersistenceMu.Lock()
	defer smsPersistenceMu.Unlock()
	if err := ctx.Err(); err != nil {
		return errors.Join(errSMSTrashPurge, err)
	}
	s.smsMu.Lock()
	for _, job := range entry.Jobs {
		if job == nil || job.ID == "" {
			s.smsMu.Unlock()
			return errSMSTrashPurge
		}
		_, active := s.jobs.Get(job.ID)
		if active || s.smsLeases[job.ID] != nil || s.smsAcquiring[job.ID] || s.smsCompleting[job.ID] {
			s.smsMu.Unlock()
			return errSMSTrashBusy
		}
	}
	s.smsMu.Unlock()
	if s.monitorVault == nil {
		return errSMSTrashPurge
	}
	for _, job := range entry.Jobs {
		if err := credentials.DeleteContext(ctx, s.monitorVault, smsCredentialID(job.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return jobs.TrashCleanupFailure(jobs.ErrTrashSMSCleanup, errors.Join(errSMSTrashPurge, err, ctx.Err()))
		}
	}
	return nil
}
