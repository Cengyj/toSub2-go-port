package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"time"

	"tosub2-go-port/internal/protocol"
)

// Capture both files before acquiring the ordinary account lease, then verify
// them again while owning it. A sealed intent can be the sole successful
// receipt, or can rely on its matching result when the final intent write was
// interrupted. Neither is a resumable authentication session.
type passwordRecoveryEvidence struct {
	path       string
	result     []byte
	intent     []byte
	hasResult  bool
	hasIntent  bool
	completion *protocol.PasswordAddRecovery
}

func readPasswordRecoveryEvidence(path, email string) (passwordRecoveryEvidence, error) {
	e := passwordRecoveryEvidence{path: path}
	var err error
	e.result, err = readRegularJSON(path)
	e.hasResult = err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return e, errors.New("密码操作记录无法读取，未覆盖凭据，私有结果已保留")
	}
	e.intent, err = readRegularJSON(protocol.PasswordAddIntentPath(path))
	e.hasIntent = err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return e, errors.New("密码派发记录无法读取，未覆盖凭据，私有结果已保留")
	}
	if e.hasIntent {
		e.completion, err = protocol.ReadPasswordAddRecovery(path, email)
		return e, err
	}
	if !e.hasResult {
		return e, nil
	}
	var legacy struct {
		Version     int        `json:"version"`
		OperationID string     `json:"operation_id"`
		Email       string     `json:"email"`
		Password    string     `json:"password"`
		AddedAt     *time.Time `json:"added_at"`
	}
	if json.Unmarshal(e.result, &legacy) != nil || legacy.Version > 1 || !strings.EqualFold(legacy.Email, email) || len(legacy.Password) < 12 {
		return e, errors.New("历史密码操作记录无效或与账号不符，未覆盖凭据，私有结果已保留")
	}
	e.completion = &protocol.PasswordAddRecovery{Version: legacy.Version, OperationID: legacy.OperationID, Email: legacy.Email, Password: legacy.Password, State: "succeeded"}
	if legacy.AddedAt != nil {
		e.completion.AddedAt = legacy.AddedAt.Format(time.RFC3339Nano)
	}
	return e, nil
}

func (e passwordRecoveryEvidence) observed() bool { return e.hasResult || e.hasIntent }

func (e passwordRecoveryEvidence) verify(email string) error {
	current, err := readPasswordRecoveryEvidence(e.path, email)
	if err != nil || e.hasResult != current.hasResult || e.hasIntent != current.hasIntent || !bytes.Equal(e.result, current.result) || !bytes.Equal(e.intent, current.intent) || !reflect.DeepEqual(e.completion, current.completion) {
		return errors.New("密码操作记录无法读取或已变化，未覆盖凭据，私有结果已保留")
	}
	return nil
}
