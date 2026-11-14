// session-cleanup is an offline, explicitly scoped removal tool. It never logs
// in, sends requests, starts services, or prints private payloads.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tosub2-go-port/internal/authmaterial"
	"tosub2-go-port/internal/credentials"
	"tosub2-go-port/internal/privatefile"
	"tosub2-go-port/internal/privatestate"
)

type envelope struct {
	Format string `json:"private_state"`
	privatestate.Binding
	Ciphertext []byte `json:"ciphertext"`
}

func stateRoot(path string, purpose string) string {
	dir := filepath.Dir(path)
	if strings.HasPrefix(purpose, "auth-only-") {
		return dir
	}
	for p := dir; p != filepath.Dir(p); p = filepath.Dir(p) {
		if strings.HasPrefix(filepath.Base(p), "job-") || filepath.Base(p) == "account-sessions" {
			return filepath.Dir(p)
		}
	}
	return ""
}
func aad(e envelope, root, path string) []byte {
	rel, _ := filepath.Rel(root, path)
	b, _ := json.Marshal(struct {
		Format string `json:"format"`
		privatestate.Binding
		Path string `json:"path"`
	}{e.Format, e.Binding, filepath.ToSlash(rel)})
	return b
}

func decodeDocument(data []byte) (any, error) {
	if err := authmaterial.ValidateUnambiguousJSON(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func decodeObject(data []byte, target *map[string]any) error {
	value, err := decodeDocument(data)
	if err != nil {
		return err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("JSON object required")
	}
	*target = object
	return nil
}

// Session fields are removed only from recognized authentication containers or
// operation journals. An enrollment session_id is factor evidence and remains.
var sessionFields = []string{"cookies", "cookie_state", "cookie_ref", "transport_session", "preparation_checkpoint", "recovery_checkpoint", "reauthentication_previous_checkpoint", "previous_session_ref", "session_ref", "initial_login_progress", "disable_reauthentication", "enrollment_reauthentication", "reauthentication_session_rejection", "reauthentication_renewals"}

func hasBusinessMaterial(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(k))
			switch normalized {
			case "accesstoken", "refreshtoken", "idtoken", "password", "pendingpassword", "totpsecret", "pendingtotpsecret", "pendingtotpuri", "otpauthuri", "secret", "tokenresponse", "apikey", "mailapiurl", "mailheaders", "mailbody", "proxyurl":
				if v != nil && v != "" {
					return true
				}
			}
			if hasBusinessMaterial(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if hasBusinessMaterial(v) {
				return true
			}
		}
	case string:
		encoded := []byte(strings.TrimSpace(x))
		if len(encoded) > 0 && (encoded[0] == '{' || encoded[0] == '[') {
			value, err := decodeDocument(encoded)
			// Ambiguous encoded data cannot be safely classified for deletion.
			if err != nil || hasBusinessMaterial(value) {
				return true
			}
		}
	}
	return false
}

func knownSessionShape(key string, value any) bool {
	if value == nil {
		return true
	}
	switch key {
	case "cookies":
		_, ok := value.([]any)
		return ok
	case "web", "oauth", "snapshot", "login", "transport", "cookie_state", "cookie_ref", "transport_session", "preparation_checkpoint", "recovery_checkpoint", "reauthentication_previous_checkpoint", "previous_session_ref", "session_ref", "initial_login_progress":
		_, ok := value.(map[string]any)
		return ok
	}
	return true
}

func sanitize(m map[string]any, purpose string) (bool, bool, error) {
	isCheckpoint := m["stage"] != nil && m["email"] != nil && (m["cookies"] != nil || m["cookie_state"] != nil || m["cookie_ref"] != nil)
	pure := isCheckpoint || purpose == "account-session" || purpose == "account-session-pointer" || purpose == "auth-only-checkpoint" || purpose == "auth-only-cookie-snapshot"
	if pure {
		// Never guess whether a completed token/password/seed in a mixed old
		// container can be discarded. Such an item needs normal-store transfer.
		fields := "version stage email authorization_method updated_at chatgpt_base auth_base warning cookies cookie_state cookie_ref web oauth transport_session account_id operation_id"
		switch purpose {
		case "account-session":
			fields = "version origin revision login email chatgpt_base auth_base session_id operation_id authorization_method snapshot transport device_id"
		case "account-session-pointer":
			fields = "version email active receipts"
		}
		allowed := map[string]bool{}
		for _, field := range strings.Fields(fields) {
			allowed[field] = true
		}
		for field, value := range m {
			if !allowed[field] || !knownSessionShape(field, value) {
				return false, false, fmt.Errorf("unclassified session-container field requires review")
			}
		}
		if hasBusinessMaterial(m) {
			return false, false, fmt.Errorf("mixed session contains business credentials requiring explicit transfer")
		}
		return true, true, nil
	}
	if purpose != "totp-reset" && m["reset_totp_requested"] == nil && m["activation_succeeded"] == nil {
		if residualSessionMaterial(m) {
			return false, false, fmt.Errorf("unclassified embedded session material requires structural review")
		}
		return false, false, nil
	}
	changed := false
	for _, k := range sessionFields {
		if v, ok := m[k]; ok {
			if !knownSessionShape(k, v) {
				return false, false, fmt.Errorf("unclassified session-field structure requires review")
			}
			if hasBusinessMaterial(v) {
				return false, false, fmt.Errorf("session field contains business credentials requiring explicit transfer")
			}
			delete(m, k)
			changed = true
		}
	}
	if residualSessionMaterial(m) {
		return false, false, fmt.Errorf("unclassified embedded session material requires structural review")
	}
	return changed, false, nil
}

func residualSessionMaterial(value any) bool {
	encoded, err := json.Marshal(value)
	if err != nil || authmaterial.ContainsLoginSessionJSON(encoded) {
		return true
	}
	return residualLegacyFields(value)
}

func residualLegacyFields(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			for _, forbidden := range append(sessionFields, "authSessionLoggingId", "codeVerifier", "code_verifier", "callbackUrl", "callback_url") {
				if key == forbidden && child != nil {
					return true
				}
			}
			if key == "body_base64" {
				encoded, _ := child.(string)
				if body, err := base64.StdEncoding.DecodeString(encoded); err == nil {
					if json.Valid(body) && authmaterial.ContainsLoginSessionJSON(body) {
						return true
					}
					var object any
					if json.Unmarshal(body, &object) == nil && residualSessionMaterial(object) {
						return true
					}
				}
			}
			if residualLegacyFields(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if residualLegacyFields(child) {
				return true
			}
		}
	}
	return false
}
func main() {
	service := flag.String("service", "", "exact existing Keychain namespace")
	credRoot := flag.String("credentials", "", "existing credential root (never account credential writes)")
	apply := flag.Bool("apply", false, "delete classified session files and sanitize mixed journals in place")
	flag.Parse()
	if *service == "" || *credRoot == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "explicit service, credential root and scan roots are required")
		os.Exit(2)
	}
	os.Setenv("TOSUB2_MAC_KEYCHAIN_SERVICE", *service)
	os.Unsetenv("TOSUB2_MAC_CREDENTIAL_ROOT")
	vault, protectorErr := credentials.NewSecureFileStore(*credRoot)
	counts := map[string]int{}
	failures := []string{}
	changedPaths := []string{}
	preserved := map[string][32]byte{}
	for _, root := range flag.Args() {
		filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				failures = append(failures, path+": unreadable")
				return nil
			}
			if d.IsDir() {
				if d.Name() == ".git" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				info, statErr := os.Stat(path)
				if statErr != nil || info.IsDir() || filepath.Ext(path) == ".json" || filepath.Ext(path) == ".cred" {
					failures = append(failures, path+": symbolic link not followed; target requires explicit scope")
				}
				return nil
			}
			if filepath.Ext(path) == ".cred" || d.Name() == "sub2api-import-oauth.json" {
				b, e := os.ReadFile(path)
				if e == nil {
					preserved[path] = sha256.Sum256(b)
				} else {
					failures = append(failures, path+": business credential unreadable")
				}
			}
			if filepath.Ext(path) != ".json" {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				failures = append(failures, path+": JSON file unreadable")
				return nil
			}
			value, err := decodeDocument(b)
			if err != nil {
				failures = append(failures, path+": invalid or ambiguous JSON requires review")
				return nil
			}
			m, object := value.(map[string]any)
			if !object {
				if residualSessionMaterial(value) {
					failures = append(failures, path+": unclassified embedded session material requires structural review")
				}
				return nil
			}
			var e envelope
			var r string
			if m["private_state"] != nil {
				if protectorErr != nil {
					failures = append(failures, path+": existing protector unavailable on this host")
					return nil
				}
				if json.Unmarshal(b, &e) != nil || e.Format != privatestate.Format {
					failures = append(failures, path+": unknown envelope")
					return nil
				}
				r = stateRoot(path, e.Purpose)
				if r == "" {
					failures = append(failures, path+": unknown binding root")
					return nil
				}
				plain, err := vault.OpenContext(context.Background(), e.Identity, e.Ciphertext, aad(e, r, path))
				if err != nil {
					failures = append(failures, path+": protected payload unavailable")
					return nil
				}
				m = nil
				if decodeObject(plain, &m) != nil {
					failures = append(failures, path+": invalid protected JSON")
					return nil
				}
			}
			changed, remove, err := sanitize(m, e.Purpose)
			if err != nil {
				failures = append(failures, path+": "+err.Error())
				return nil
			}
			if !changed {
				return nil
			}
			action := "sanitize journal"
			if remove {
				action = "delete session"
			}
			counts[action]++
			if *apply {
				// Refuse a concurrent service write rather than overwrite newer data.
				current, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(current, b) {
					failures = append(failures, path+": changed during cleanup")
					return nil
				}
				if remove {
					err = privatefile.Remove(context.Background(), path)
				} else {
					plain, _ := json.MarshalIndent(m, "", "  ")
					clean := plain
					if e.Format != "" {
						e.Ciphertext, err = vault.SealContext(context.Background(), e.Identity, plain, aad(e, r, path))
						if err == nil {
							clean, err = json.MarshalIndent(e, "", "  ")
						}
					}
					if err == nil {
						err = privatefile.Write(context.Background(), path, append(clean, '\n'))
					}
					if err == nil {
						readback, re := os.ReadFile(path)
						if re != nil {
							err = re
						} else if e.Format != "" {
							var saved envelope
							if json.Unmarshal(readback, &saved) != nil {
								err = fmt.Errorf("invalid readback")
							} else {
								var opened []byte
								opened, err = vault.OpenContext(context.Background(), saved.Identity, saved.Ciphertext, aad(saved, r, path))
								if err == nil && !bytes.Equal(opened, plain) {
									err = fmt.Errorf("protected readback mismatch")
								}
							}
						} else if !bytes.Equal(bytes.TrimSpace(readback), plain) {
							err = fmt.Errorf("readback mismatch")
						}
					}
				}
				if err != nil {
					failures = append(failures, path+": cleanup or readback failed")
					return nil
				}
			}
			changedPaths = append(changedPaths, action+": "+path)
			return nil
		})
	}
	for path, before := range preserved {
		after, e := os.ReadFile(path)
		if e != nil || sha256.Sum256(after) != before {
			failures = append(failures, path+": business credential changed during scan")
		}
	}
	out, _ := json.MarshalIndent(map[string]any{"applied": *apply, "counts": counts, "paths": changedPaths, "preserved_credential_and_token_files": len(preserved), "unresolved": failures}, "", "  ")
	fmt.Println(string(out))
	if len(failures) > 0 {
		os.Exit(1)
	}
}
