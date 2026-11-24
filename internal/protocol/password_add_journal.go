package protocol

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"time"

	"tosub2-go-port/internal/privatestate"
)

var ErrPasswordAddUnconfirmed = errors.New("PASSWORD_ADD_RESULT_UNKNOWN: 已存在未确认的添加密码请求；候选密码和操作证据已保留，禁止再次提交远端修改")

// PasswordAddRecovery is private operation evidence, never a public status or
// a resumable login. The password is eligible for a local commit only when the
// operation has a confirmed success receipt.
type PasswordAddRecovery struct {
	Version     int    `json:"version"`
	OperationID string `json:"operation_id"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	State       string `json:"state"`
	AddedAt     string `json:"added_at,omitempty"`
}

func PasswordAddIntentPath(resultPath string) string { return resultPath + ".intent" }

func readPasswordAddIntent(resultPath, email string) (PasswordAddRecovery, error) {
	ctx, cancel := privateStateSaveContext()
	defer cancel()
	data, binding, err := privatestate.ReadJSONWithBinding(ctx, PasswordAddIntentPath(resultPath), "password-add", 64<<10)
	if err != nil {
		return PasswordAddRecovery{}, err
	}
	var record PasswordAddRecovery
	fields, unambiguous := recentAuthObject(data)
	for key := range fields {
		switch key {
		case "version", "operation_id", "email", "password", "state", "added_at":
		default:
			unambiguous = false
		}
	}
	if !unambiguous || json.Unmarshal(data, &record) != nil || record.Version != 1 || record.OperationID == "" || strings.TrimSpace(email) == "" || !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(record.Email)) || !validNewPassword(record.Password) {
		return PasswordAddRecovery{}, errors.New("PASSWORD_ADD_EVIDENCE_INVALID: 添加密码的操作证据无效或身份不一致，未修改原记录")
	}
	if binding.Schema != 0 && (binding.Identity != privatestate.Identity(record.Email) || binding.Operation != record.OperationID) {
		return PasswordAddRecovery{}, errors.New("PASSWORD_ADD_EVIDENCE_INVALID: 添加密码的保护存储绑定不一致")
	}
	switch record.State {
	case "may_have_been_sent", "not_sent", "rejected":
		if record.AddedAt != "" {
			return PasswordAddRecovery{}, ErrPasswordAddUnconfirmed
		}
	case "succeeded":
		if _, err := time.Parse(time.RFC3339Nano, record.AddedAt); err != nil {
			return PasswordAddRecovery{}, ErrPasswordAddUnconfirmed
		}
	default:
		return PasswordAddRecovery{}, ErrPasswordAddUnconfirmed
	}
	return record, nil
}

// ReadPasswordAddRecovery returns only confirmed local completion material.
// Definite non-dispatch/rejection permits another explicit operation, whereas
// unknown dispatch blocks all later password/add calls across process restarts.
func ReadPasswordAddRecovery(resultPath, email string) (*PasswordAddRecovery, error) {
	if resultPath == "" {
		return nil, nil
	}
	record, err := readPasswordAddIntent(resultPath, email)
	if errors.Is(err, os.ErrNotExist) {
		if _, resultErr := os.Lstat(resultPath); !errors.Is(resultErr, os.ErrNotExist) {
			return nil, errors.New("PASSWORD_ADD_PREVIOUS_RESULT: 已存在添加密码结果，须先完成本地恢复，不能覆盖旧证据")
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record.State == "succeeded" {
		return &record, nil
	}
	if record.State == "not_sent" || record.State == "rejected" {
		return nil, nil
	}
	// If updating the sealed receipt failed, an independently saved success
	// file can still prove this exact operation. Historical success without
	// the matching operation ID must never release the unknown-write guard.
	ctx, cancel := privateStateSaveContext()
	defer cancel()
	data, readErr := privatestate.ReadJSON(ctx, resultPath, "password-add", 64<<10)
	var completion struct {
		Version     int    `json:"version"`
		OperationID string `json:"operation_id"`
		Email       string `json:"email"`
		Password    string `json:"password"`
		AddedAt     string `json:"added_at"`
	}
	if readErr == nil {
		fields, valid := recentAuthObject(data)
		// Legacy result extensions are retained, but Go's case-insensitive
		// struct decoder must not let an alias override an operation binding.
		for key := range fields {
			for _, canonical := range []string{"version", "operation_id", "email", "password", "added_at"} {
				if key != canonical && strings.EqualFold(key, canonical) {
					valid = false
				}
			}
		}
		if valid && json.Unmarshal(data, &completion) == nil {
			_, dateErr := time.Parse(time.RFC3339Nano, completion.AddedAt)
			if completion.Version == 1 && completion.OperationID == record.OperationID && completion.Email == record.Email && completion.Password == record.Password && dateErr == nil {
				record.State, record.AddedAt = "succeeded", completion.AddedAt
				return &record, nil
			}
		}
	}
	return nil, ErrPasswordAddUnconfirmed
}

func writePasswordAddIntent(resultPath string, record PasswordAddRecovery) error {
	if resultPath == "" {
		return nil
	}
	ctx, cancel := privateStateSaveContext()
	defer cancel()
	if err := privatestate.WriteJSON(ctx, PasswordAddIntentPath(resultPath), "password-add", record.Email, record.OperationID, record); err != nil {
		return err
	}
	stored, err := readPasswordAddIntent(resultPath, record.Email)
	if err != nil || !reflect.DeepEqual(stored, record) {
		return errors.New("PASSWORD_ADD_EVIDENCE_READBACK_FAILED: 添加密码操作证据保存后未能确认读回，未继续远端操作")
	}
	return nil
}

func RemovePasswordAddEvidence(resultPath string) error {
	if resultPath == "" {
		return nil
	}
	// Remove the dispatch guard first. If the final intent update failed, the
	// matching success result may be the only confirmation; interruption must
	// not discard that receipt while leaving a now-unprovable unknown guard.
	if err := RemovePrivateResult(PasswordAddIntentPath(resultPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := RemovePrivateResult(resultPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
