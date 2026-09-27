package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/jobs"
	"tosub2-go-port/internal/providers"
)

var smsPersistenceMu sync.Mutex

type privateSMSState struct {
	Version      int                        `json:"version"`
	JobID        string                     `json:"jobId"`
	Email        string                     `json:"email"`
	Provider     string                     `json:"provider"`
	APIKey       string                     `json:"apiKey,omitempty"`
	Base         string                     `json:"base,omitempty"`
	Service      string                     `json:"service,omitempty"`
	ServiceLabel string                     `json:"serviceLabel,omitempty"`
	Country      string                     `json:"country,omitempty"`
	MaxPrice     string                     `json:"maxPrice,omitempty"`
	Lease        providers.NumberLease      `json:"lease"`
	Seen         map[string]bool            `json:"seen"`
	PendingCode  string                     `json:"pendingCode,omitempty"`
	PollStopped  bool                       `json:"pollStopped,omitempty"`
	Deadline     time.Time                  `json:"deadline"`
	Action       string                     `json:"action,omitempty"`
	Custom       *providers.CustomPoolState `json:"custom,omitempty"`
}

func smsCredentialID(id string) string { return "sms-job-" + id }
func (s *Server) smsCredentialContext() (context.Context, context.CancelFunc) {
	s.smsMu.Lock()
	parent := s.smsShutdownContext
	s.smsMu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, 3*time.Second)
}
func (s *Server) smsVaultLoad(id string) (credentials.Credentials, error) {
	if s.monitorVault == nil {
		return credentials.Credentials{}, credentials.ErrUnavailable
	}
	ctx, cancel := s.smsCredentialContext()
	defer cancel()
	if v, ok := s.monitorVault.(credentials.ContextStore); ok {
		return v.LoadContext(ctx, id)
	}
	return s.monitorVault.Load(id)
}
func (s *Server) smsVaultSave(id string, v credentials.Credentials) error {
	if s.monitorVault == nil {
		return credentials.ErrUnavailable
	}
	ctx, cancel := s.smsCredentialContext()
	defer cancel()
	if vault, ok := s.monitorVault.(credentials.ContextStore); ok {
		return vault.SaveContext(ctx, id, v)
	}
	return s.monitorVault.Save(id, v)
}
func (s *Server) smsVaultDelete(id string) error {
	if s.monitorVault == nil {
		return credentials.ErrUnavailable
	}
	ctx, cancel := s.smsCredentialContext()
	defer cancel()
	if vault, ok := s.monitorVault.(credentials.ContextStore); ok {
		return vault.DeleteContext(ctx, id)
	}
	return s.monitorVault.Delete(id)
}
func (s *Server) snapshotSMSLocked(id string, l *smsLease) (privateSMSState, error) {
	state := privateSMSState{Version: 1, JobID: id, Lease: l.lease, Seen: map[string]bool{}, Deadline: l.deadline, PendingCode: l.pendingCode, PollStopped: l.pollStopped, Action: l.action}
	if j, ok := s.jobs.Get(id); ok {
		state.Email = j.Email
		state.ServiceLabel = j.SMSServiceLabel
		if j.SMSStatus == "verified" {
			state.Action = "complete"
		}
	}
	for code, seen := range l.seen {
		state.Seen[code] = seen
	}
	switch p := l.provider.(type) {
	case *providers.HTTPProvider:
		state.Provider = "luban"
		state.APIKey = p.APIKey
		state.Base = p.Base
		state.Service = p.Service
	case *providers.SMSBowerProvider:
		state.Provider = "smsbower"
		state.APIKey = p.APIKey
		state.Base = p.Base
		state.Service = p.Service
		state.Country = p.Country
		state.MaxPrice = p.MaxPrice
	case *providers.CustomPool:
		state.Provider = "custom"
		snapshot := p.PrivateState()
		state.Custom = &snapshot
	default:
		return state, errors.New("SMS provider cannot persist lease state")
	}
	return state, nil
}
func (s *Server) writeSMSState(state privateSMSState) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.smsVaultSave(smsCredentialID(state.JobID), credentials.Credentials{Password: string(b)})
}
func (s *Server) saveSMSLease(id string, l *smsLease) error {
	smsPersistenceMu.Lock()
	defer smsPersistenceMu.Unlock()
	s.smsMu.Lock()
	if s.smsLeases[id] != l {
		s.smsMu.Unlock()
		return nil
	}
	state, err := s.snapshotSMSLocked(id, l)
	s.smsMu.Unlock()
	if err != nil {
		return err
	}
	return s.writeSMSState(state)
}
func (s *Server) deleteSMSLeaseState(id, leaseID string) {
	smsPersistenceMu.Lock()
	defer smsPersistenceMu.Unlock()
	c, err := s.smsVaultLoad(smsCredentialID(id))
	if err != nil {
		return
	}
	var state privateSMSState
	if json.Unmarshal([]byte(c.Password), &state) == nil && state.Lease.ID == leaseID {
		_ = s.smsVaultDelete(smsCredentialID(id))
	}
}
func (s *Server) suspendSMS(id string, l *smsLease) {
	smsPersistenceMu.Lock()
	defer smsPersistenceMu.Unlock()
	s.smsMu.Lock()
	if s.smsLeases[id] != l {
		s.smsMu.Unlock()
		return
	}
	state, err := s.snapshotSMSLocked(id, l)
	delete(s.smsLeases, id)
	l.cancel()
	s.smsMu.Unlock()
	if err == nil {
		err = s.writeSMSState(state)
	}
	if err != nil {
		s.jobs.UpdateSMS(id, l.lease.Provider, l.lease.Provider, l.lease.Number, "interrupted", "当前系统无法安全恢复接码订单，请重新配置接码或手动输入")
	}
}
func (s *Server) smsRestoreFailed(job *jobs.Job, reason string) {
	// Completed orders normally have no remaining private record. Only stale
	// unfinished state needs a warning and must stop looking like live polling.
	switch job.SMSStatus {
	case "", "idle", "unavailable", "completed":
		return
	}
	s.jobs.UpdateSMS(job.ID, job.SMSProviderID, job.SMSProviderName, job.CurrentPhone, "error", reason+"，自动收短信已停止，可手动输入验证码或换号")
}

func (s *Server) restoreSMSLeases() {
	for _, job := range s.jobs.List() {
		if job.SMSProviderID == "" {
			s.smsRestoreFailed(job, "接码订单无法从系统凭据服务恢复")
			continue
		}
		c, err := s.smsVaultLoad(smsCredentialID(job.ID))
		if err != nil {
			s.smsRestoreFailed(job, "接码订单无法从系统凭据服务恢复")
			continue
		}
		var state privateSMSState
		if json.Unmarshal([]byte(c.Password), &state) != nil || state.Version != 1 || state.JobID != job.ID || !strings.EqualFold(state.Email, job.Email) || strings.TrimSpace(state.Lease.ID) == "" || strings.TrimSpace(state.Lease.Number) == "" || state.Provider != job.SMSProviderID || state.Lease.Provider != state.Provider || (state.Action != "" && state.Action != "release" && state.Action != "complete") {
			s.smsRestoreFailed(job, "接码订单恢复数据无效")
			continue
		}
		var p providers.SmsProvider
		switch state.Provider {
		case "luban":
			p = &providers.HTTPProvider{ID: "luban", APIKey: state.APIKey, Base: state.Base, Service: state.Service}
		case "smsbower":
			provider := providers.NewSMSBowerProvider(state.APIKey, state.Base, state.Service, state.Country)
			provider.MaxPrice = state.MaxPrice
			p = provider
		case "custom":
			if state.Custom != nil {
				pool, e := s.customSMSPool(state.Custom.Entries)
				if e == nil {
					e = pool.RestorePrivateState(*state.Custom)
				}
				if e == nil {
					p = pool
				}
			}
		}
		if p == nil {
			s.smsRestoreFailed(job, "接码订单恢复数据无效")
			continue
		}
		if state.Action == "" && (job.Status == jobs.StatusCanceled || job.Status == jobs.StatusFailed || job.Status == jobs.StatusSucceeded) {
			if job.SMSStatus == "verified" || job.SMSStatus == "completed" {
				state.Action = "complete"
			} else {
				state.Action = "release"
			}
		}
		if job.SMSServiceLabel == "" {
			label := state.ServiceLabel
			if label == "" {
				switch state.Provider {
				case "luban":
					label = state.Service
				case "smsbower":
					label = state.Service + " / " + state.Country
				case "custom":
					label = fmt.Sprintf("%d 个号码", len(state.Custom.Entries))
				}
			}
			s.jobs.UpdateSMS(job.ID, job.SMSProviderID, job.SMSProviderName, job.CurrentPhone, job.SMSStatus, job.SMSError, jobs.SanitizeSMSServiceLabel(label, state.APIKey))
		}
		s.installSMSLeaseState(job.ID, p, state.Lease, &state)
	}
}

func (s *Server) saveCustomSMSPool(pool *providers.CustomPool) error {
	smsPersistenceMu.Lock()
	defer smsPersistenceMu.Unlock()
	state := pool.PrivateState()
	hash := sha256.Sum256([]byte(strings.Join(state.Entries, "\n")))
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.smsVaultSave("sms-pool-"+hex.EncodeToString(hash[:]), credentials.Credentials{Password: string(b)})
}
