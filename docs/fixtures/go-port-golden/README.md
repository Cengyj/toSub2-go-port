# Go port golden fixtures

These fixtures capture the version-1 wire formats observed in src/protocol-login.mjs and src/console-server.mjs. Values are synthetic and safe for tests; they are not exported from a real account. Tests must compare structure, required keys, redaction, and round-trip behavior rather than timestamps or token values.

Fixtures:

- sub2api-import-oauth.json: sub2api-data export/import payload.
- login-checkpoint.json: private checkpoint with cookie jar and OAuth PKCE/phone state.
- job-meta.json: public task metadata written beside each task output.
- totp-setup-result.json: private TOTP activation hand-off file.
- password-add-result.json: private password activation hand-off file.
- sub2api-monitor.json: persisted Sub2API monitor configuration and state.

adminApiKey, access/refresh/id tokens, passwords, TOTP secrets, cookies, and phone values are placeholders only. Never replace them with real credentials.
