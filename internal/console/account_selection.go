package console

import "time"

// accountSelection is the opt-in cross-page command index. It deliberately
// carries identity, operation generation and final capabilities, not row
// diagnostics or workflow presentation. Visible rows keep their full contract.
// Explicit false values are part of this contract and must not be omitted.
type accountSelection struct {
	ID                        string    `json:"id"`
	Email                     string    `json:"email"`
	CreatedAt                 time.Time `json:"createdAt"`
	LastOperationAt           time.Time `json:"lastOperationAt"`
	Attempt                   int       `json:"attempt"`
	AuthorizationMethod       string    `json:"authorizationMethod"`
	Status                    string    `json:"status"`
	OperationStatus           string    `json:"operationStatus"`
	CanResume                 bool      `json:"canResume"`
	CanDownload               bool      `json:"canDownload"`
	CanRegenerate             bool      `json:"canRegenerate"`
	CanRetry                  bool      `json:"canRetry"`
	CanForceRelogin           bool      `json:"canForceRelogin"`
	CanSetupTOTP              bool      `json:"canSetupTotp"`
	CanResetTOTP              bool      `json:"canResetTotp"`
	CanAddPassword            bool      `json:"canAddPassword"`
	CanCompleteTOTP           bool      `json:"canCompleteTotp"`
	TOTPExportBlocked         bool      `json:"totpExportBlocked"`
	TOTPRecoveryPending       bool      `json:"totpRecoveryPending"`
	TOTPCredentialInvalidated bool      `json:"totpCredentialInvalidated"`
	TOTPCompletionPending     bool      `json:"totpCompletionPending"`
	TOTPWorkflowStatus        string    `json:"totpWorkflowStatus"`
}

// Consume the final public projection, including live journal gates. Do not
// infer permissions again from Job or historical authorization availability.
func compactAccountSelection(view map[string]any) accountSelection {
	return accountSelection{
		ID: view["id"].(string), Email: view["email"].(string),
		CreatedAt: view["createdAt"].(time.Time), LastOperationAt: view["lastOperationAt"].(time.Time),
		Attempt: view["attempt"].(int), AuthorizationMethod: view["authorizationMethod"].(string),
		Status: view["status"].(string), OperationStatus: view["operationStatus"].(string),
		CanResume: view["canResume"].(bool), CanDownload: view["canDownload"].(bool),
		CanRegenerate: view["canRegenerate"].(bool), CanRetry: view["canRetry"].(bool),
		CanForceRelogin: view["canForceRelogin"].(bool), CanSetupTOTP: view["canSetupTotp"].(bool),
		CanResetTOTP: view["canResetTotp"].(bool), CanAddPassword: view["canAddPassword"].(bool),
		CanCompleteTOTP: view["canCompleteTotp"].(bool), TOTPExportBlocked: view["totpExportBlocked"].(bool),
		TOTPRecoveryPending: view["totpRecoveryPending"].(bool), TOTPCredentialInvalidated: view["totpCredentialInvalidated"].(bool),
		TOTPCompletionPending: view["totpCompletionPending"].(bool), TOTPWorkflowStatus: view["totpWorkflowStatus"].(string),
	}
}
