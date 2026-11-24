package jobs

import (
	"errors"
	"reflect"
	"time"
)

var ErrCredentialReadSuperseded = errors.New("账号或安全凭据在读取期间已变化，请重新读取当前资料")

// Jobs are updated in place. Pointer identity alone cannot distinguish a
// delayed vault read from a completed password/TOTP replacement on that job.
// Input comparison also covers historical same-email rows whose current seed
// is replaced while retaining their old operation ID and timestamps.
type credentialReadVersion struct {
	email, operation, resetOperation string
	created, operationAt, updated    time.Time
	resetCommitted, resetPending     bool
	invalidated                      bool
	input                            map[string]any
}

func captureCredentialReadVersion(job *Job) credentialReadVersion {
	return credentialReadVersion{
		email: job.Email, operation: job.Operation, resetOperation: job.TOTPResetOperationID,
		created: job.CreatedAt, operationAt: job.LastOperationAt, updated: job.UpdatedAt,
		resetCommitted: job.TOTPResetCommitted, resetPending: job.TOTPRecoveryPending, invalidated: job.TOTPCredentialInvalidated,
		input: cloneMap(job.Input),
	}
}

func (v credentialReadVersion) matches(job *Job) bool {
	return job != nil && job.Email == v.email && job.Operation == v.operation && job.TOTPResetOperationID == v.resetOperation &&
		job.CreatedAt.Equal(v.created) && job.LastOperationAt.Equal(v.operationAt) && job.UpdatedAt.Equal(v.updated) &&
		job.TOTPResetCommitted == v.resetCommitted && job.TOTPRecoveryPending == v.resetPending && job.TOTPCredentialInvalidated == v.invalidated &&
		reflect.DeepEqual(job.Input, v.input)
}
