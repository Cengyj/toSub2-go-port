package main

// The SDK sees the same authentication page context as the original client.
// Password reset and password creation are separate Sentinel flow contexts.
func sentinelPagePath(flow string) string {
	switch flow {
	case "email_otp_validate":
		return "/email-verification"
	case "oauth_create_account":
		return "/about-you"
	case "password_verify":
		return "/log-in/password"
	case "username_password_create":
		return "/create-account/password"
	case "password_reset":
		return "/reset-password/new-password"
	default:
		return "/log-in"
	}
}
