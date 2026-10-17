package console

import "tosub2-go-port/internal/sub2api"

// ParseTOTPSub2APIConfig preserves the console adapter's configuration contract.
// Validation belongs to Sub2API and is shared by HTTP and application workflows.
func ParseTOTPSub2APIConfig(value any) (sub2api.Config, sub2api.UploadOptions, error) {
	return sub2api.ParseUploadSettings(value)
}
