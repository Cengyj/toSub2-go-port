package console

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"unicode"
)

var removedAuthenticationInputKeyNormalizer = strings.NewReplacer("_", "", "-", "")

var (
	errRemovedAuthenticationInputs    = errors.New("登录会话保存、恢复和 Cookie 导出选项已移除")
	errRemovedRemoteAuthentication    = errors.New("当前操作不支持恢复远端会话或重新验证身份")
	errRemovedAuthenticationOperation = errors.New("旧版远端恢复与核实操作已移除")
)

// Reject removed authentication persistence inputs by presence, including null
// and false. These are never interpreted as fresh-operation preferences.
func rejectRemovedAuthenticationInputs[V any](values map[string]V) error {
	for key := range values {
		// Struct-backed requests use encoding/json's case-insensitive field
		// matching. Apply the same refusal to map-backed single-job actions,
		// including old snake/hyphen aliases, instead of silently ignoring them.
		switch strings.ToLower(removedAuthenticationInputKeyNormalizer.Replace(key)) {
		case "reauthenticate":
			return errRemovedRemoteAuthentication
		case "checkpointpath", "resumecheckpoint", "checkpoint", "sessionoutputpath", "sessionpath", "webonly", "nosub2apiexport", "outputmode",
			"cookie", "cookies", "cookieref", "cookiestate", "callback", "callbackurl", "authorizationcode", "codeverifier", "codechallenge", "oauthstate", "loginsession", "logincheckpoint":
			return errRemovedAuthenticationInputs
		}
	}
	return nil
}

func rejectRemovedAuthenticationOperation(operation string) error {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(operation), "_", "-")) {
	case "recover-2fa", "inspect-2fa", "reauthenticate":
		return errRemovedAuthenticationOperation
	}
	return nil
}

func rejectAmbiguousRequestFieldAliases(values map[string]json.RawMessage) error {
	seen := map[string]bool{}
	for key := range values {
		// Match encoding/json's Unicode simple folding, including aliases such
		// as long-s that ordinary lowercasing does not equate with ASCII s.
		name := strings.Map(func(r rune) rune {
			for {
				next := unicode.SimpleFold(r)
				if next <= r {
					return next
				}
				r = next
			}
		}, key)
		if seen[name] {
			return errors.New("ambiguous request field aliases")
		}
		seen[name] = true
	}
	return nil
}

// Validate the request schema before decoding. Interface and RawMessage fields
// are opaque business documents: their case-sensitive vendor keys must survive.
func rejectNestedRequestFieldAliases(raw json.RawMessage, schema reflect.Type) error {
	for schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	if schema.Kind() == reflect.Interface || schema == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return errors.New("invalid JSON")
	}
	switch raw[0] {
	case '{':
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		if err := rejectAmbiguousRequestFieldAliases(fields); err != nil {
			return err
		}
		for key, value := range fields {
			if schema.Kind() == reflect.Map {
				if err := rejectNestedRequestFieldAliases(value, schema.Elem()); err != nil {
					return err
				}
			} else if schema.Kind() == reflect.Struct {
				for i := 0; i < schema.NumField(); i++ {
					field := schema.Field(i)
					name := strings.Split(field.Tag.Get("json"), ",")[0]
					if name == "" {
						name = field.Name
					}
					if field.PkgPath == "" && name != "-" && strings.EqualFold(name, key) {
						if err := rejectNestedRequestFieldAliases(value, field.Type); err != nil {
							return err
						}
						break
					}
				}
			}
		}
	case '[':
		if schema.Kind() != reflect.Slice && schema.Kind() != reflect.Array {
			return nil // The typed decoder reports this shape mismatch.
		}
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := rejectNestedRequestFieldAliases(value, schema.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
