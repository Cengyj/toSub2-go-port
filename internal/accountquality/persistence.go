package accountquality

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tosub2-go-port/internal/modelprobe"
	"tosub2-go-port/internal/privatefile"
)

const directoryName = "account-quality"
const settingsName = "settings.json"
const questionCount = modelprobe.TotalQuestions

type storedRow struct {
	SchemaVersion           int              `json:"schemaVersion"`
	AccountRevision         string           `json:"accountRevision"`
	IdentityHash            string           `json:"identityHash,omitempty"`
	OptionsHash             string           `json:"optionsHash,omitempty"`
	HTTPObservation         *HTTPObservation `json:"httpObservation,omitempty"`
	HTTPObservationRevision string           `json:"httpObservationRevision,omitempty"`
	Snapshot                Snapshot         `json:"snapshot"`
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func snapshotFile(id string) string { return digest(id) + ".json" }
func options(c Settings) modelprobe.Options {
	return modelprobe.Options{Model: c.Model, ReasoningEffort: c.ReasoningEffort, MinimumScore: c.MinimumScore, TimeoutSeconds: c.TimeoutSeconds}
}
func optionsHash(c Settings) string {
	data, _ := json.Marshal([]any{modelprobe.RuleVersion, options(c)})
	return digest(string(data))
}

// ReadSettings never creates directories or changes policy. A missing file is
// the only condition that grants the disabled default; unreadable is not off.
func ReadSettings(root string) (Settings, error) {
	if strings.TrimSpace(root) == "" {
		return Settings{}, ErrSettingsUnavailable
	}
	if info, err := os.Lstat(root); err == nil && !info.IsDir() {
		return Settings{}, ErrSettingsUnavailable
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Settings{}, ErrSettingsUnavailable
	}
	c, err := readSettingsFile(filepath.Join(root, directoryName))
	if errors.Is(err, os.ErrNotExist) {
		return DefaultSettings(), nil
	}
	if err != nil || validateSettings(c) != nil {
		return Settings{}, ErrSettingsUnavailable
	}
	c.PersistenceWarning = ""
	return c, nil
}

type policyDocument struct{ Settings Settings }

// All durable policy switches are explicit. A misspelled, missing, null or
// duplicated switch must not silently decode into a permissive false value.
func (p *policyDocument) UnmarshalJSON(data []byte) error {
	required := map[string]bool{"schemaVersion": false, "revision": false, "enabled": false, "beforeUpload": false, "blockDegraded": false, "blockFailed": false, "model": false, "reasoningEffort": false, "minimumScore": false, "timeoutSeconds": false, "cooldownSeconds": false, "resultTTLSeconds": false}
	d := json.NewDecoder(bytes.NewReader(data))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return ErrSettingsUnavailable
	}
	seen := map[string]bool{}
	for d.More() {
		tok, err = d.Token()
		if err != nil {
			return ErrSettingsUnavailable
		}
		key, ok := tok.(string)
		if !ok || seen[key] {
			return ErrSettingsUnavailable
		}
		seen[key] = true
		if _, ok := required[key]; !ok && key != "persistenceWarning" {
			return ErrSettingsUnavailable
		}
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrSettingsUnavailable
		}
		if _, ok := required[key]; ok {
			required[key] = true
		}
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim('}') {
		return ErrSettingsUnavailable
	}
	for _, present := range required {
		if !present {
			return ErrSettingsUnavailable
		}
	}
	if json.Unmarshal(data, &p.Settings) != nil {
		return ErrSettingsUnavailable
	}
	// Historical six-question policies use the same explicit document schema.
	// Accept their old threshold only while decoding; API writes validate the
	// current one-question threshold separately before committing any policy.
	return validateStoredSettings(p.Settings)
}

func readSettingsFile(root string) (Settings, error) {
	var document policyDocument
	err := readPrivateJSON(root, settingsName, &document)
	if err == nil {
		// Reading an older policy changes only its in-memory scoring threshold.
		// It never writes the file, increments its revision, or alters switches.
		document.Settings.MinimumScore = modelprobe.DefaultMinimumScore
	}
	return document.Settings, err
}

func validateStoredSettings(c Settings) error {
	if c.MinimumScore < 1 || c.MinimumScore > 6 {
		return errors.New("检测正确题数设置无效")
	}
	c.MinimumScore = modelprobe.DefaultMinimumScore
	return validateSettings(c)
}

func readPrivateJSON(root, name string, out any) error {
	dir, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !dir.IsDir() {
		return ErrSettingsUnavailable
	}
	if err := verifyDirectory(root, dir); err != nil {
		return ErrSettingsUnavailable
	}
	path := filepath.Join(root, filepath.Base(name))
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() > 1<<20 {
		return ErrSettingsUnavailable
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() || privatefile.Verify(f) != nil {
		return ErrSettingsUnavailable
	}
	d := json.NewDecoder(io.LimitReader(f, (1<<20)+1))
	if err = d.Decode(out); err != nil {
		return err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return ErrSettingsUnavailable
	}
	after, err := os.Lstat(root)
	if err != nil || !after.IsDir() || !os.SameFile(dir, after) {
		return ErrSettingsUnavailable
	}
	return nil
}

func validateSettings(c Settings) error {
	if c.SchemaVersion != 1 || c.Revision == 0 {
		return errors.New("检测设置版本无效")
	}
	if len(c.Model) > 128 || strings.ContainsAny(c.Model, "\r\n\t") || c.Enabled && strings.TrimSpace(c.Model) == "" {
		return errors.New("启用检测时必须填写有效模型名称")
	}
	switch c.ReasoningEffort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return errors.New("检测推理强度无效")
	}
	if c.MinimumScore < 1 || c.MinimumScore > questionCount {
		return errors.New("糖果题答对通过，正确题数必须为1")
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 300 {
		return errors.New("检测超时必须为1–300秒")
	}
	if c.CooldownSeconds < 0 || c.CooldownSeconds > 86400 {
		return errors.New("检测冷却必须为0–86400秒")
	}
	if c.ResultTTLSeconds < 1 || c.ResultTTLSeconds > 86400 {
		return errors.New("检测结果有效期必须为1–86400秒")
	}
	return nil
}

func cloneResult(r *modelprobe.Result) *modelprobe.Result {
	if r == nil {
		return nil
	}
	v := *r
	return &v
}
func cloneSnapshot(s Snapshot) Snapshot {
	s.LastResult = cloneResult(s.LastResult)
	s.HTTPObservation = cloneHTTPObservation(s.HTTPObservation)
	s.ProbeHistory = normalizeProbeHistory(s.ProbeHistory)
	copyTime := func(t *time.Time) *time.Time {
		if t == nil {
			return nil
		}
		v := *t
		return &v
	}
	s.LastAttempt.StartedAt = copyTime(s.LastAttempt.StartedAt)
	s.LastAttempt.FinishedAt = copyTime(s.LastAttempt.FinishedAt)
	s.NextAllowedAt = copyTime(s.NextAllowedAt)
	return s
}

func (s *Service) lockWrite(ctx context.Context) error {
	select {
	case s.writeGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Service) unlockWrite() { <-s.writeGate }
