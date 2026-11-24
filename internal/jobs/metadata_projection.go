package jobs

import (
	"reflect"
	"slices"

	"tosub2-go-port/internal/logging"
)

// metadataProjectionCache caches only the derivation of a record, never a
// commit. Every write still synchronously replaces and flushes the complete v2
// document. Comparing the entire source (including log tails and known-secret
// values) avoids a hand-maintained invalidation protocol across mutations.
// The cache is rebuilt from the retained records each time, so deletion cannot
// retain an unbounded archive of account or credential material in memory.
type metadataProjectionCache struct {
	current  map[string]metadataProjection
	previous map[string]metadataProjection
}

type metadataProjection struct {
	source  *Job
	secrets []string
	value   map[string]any
}

func (c metadataProjectionCache) begin(capacity int) metadataProjectionCache {
	return metadataProjectionCache{current: make(map[string]metadataProjection, capacity), previous: c.current}
}

func (c metadataProjectionCache) job(job *Job) map[string]any {
	if job == nil {
		return jobMetadata(nil)
	}
	source := *job
	// These private fields affect projection only through known-secret
	// redaction. Cache the redaction set, not the mutable configuration map.
	source.Input = nil
	source.recoveryCleanup = nil
	source.MailAPIURL, source.TOTPSetupSecret, source.TOTPSetupURI = "", "", ""
	redactionSecrets := jobKnownSecrets(job)
	known := slices.Clone(redactionSecrets)
	slices.Sort(known)
	known = slices.Compact(known)
	prior, exists := c.previous[job.ID]
	if exists && slices.Equal(prior.secrets, known) && reflect.DeepEqual(prior.source, &source) {
		c.current[job.ID] = prior
		return prior.value
	}
	snapshot := cloneJob(&source)
	// Diagnostics carry a few pointer fields. Safe returns their detached
	// value. An invalid legacy diagnostic deliberately misses the cache until
	// it is replaced; it can never hide a change by sharing a pointer.
	for i, entry := range snapshot.LogEntries {
		if entry.Diagnostic != nil {
			snapshot.LogEntries[i].Diagnostic = entry.Diagnostic.Safe()
		}
	}
	// A new event changes UpdatedAt and misses the whole-record cache, but
	// already retained log text is unchanged. Reuse only exact text under the
	// same account identity and complete secret set. This bounded memo exists
	// only for this call; it cannot retain removed historical lines or secrets.
	reuseLogs := exists && prior.source != nil && prior.source.ID == job.ID && prior.source.Email == job.Email && slices.Equal(prior.secrets, known)
	logs := newMetadataLogProjection(prior, reuseLogs, redactionSecrets)
	value := jobMetadataWithLogProjection(job, logs.project)
	// jobMetadata preserves the private operation receipt binding outside its
	// public projection. Do not let future in-place receipt changes mutate the
	// cached document or its source snapshot.
	value["totp_intent_receipts"] = snapshot.TOTPIntentReceipts
	c.current[job.ID] = metadataProjection{source: snapshot, secrets: known, value: value}
	return value
}

const metadataLogProjectionEntries = 512
const metadataLogProjectionBytes = 128 << 10

type metadataLogProjection struct {
	values  map[string]string
	bytes   int
	secrets []string
}

func newMetadataLogProjection(prior metadataProjection, reuse bool, secrets []string) *metadataLogProjection {
	p := &metadataLogProjection{values: make(map[string]string), secrets: secrets}
	if !reuse {
		return p
	}
	if saved, ok := prior.value["logs"].([]string); ok && len(saved) == len(prior.source.Logs) {
		for i, raw := range prior.source.Logs {
			p.remember(raw, saved[i])
		}
	}
	if saved, ok := prior.value["log_entries"].([]LogEntry); ok && len(saved) == len(prior.source.LogEntries) {
		for i, entry := range prior.source.LogEntries {
			p.remember(entry.Message, saved[i].Message)
		}
	}
	return p
}

func (p *metadataLogProjection) remember(text, redacted string) {
	if _, exists := p.values[text]; exists || len(p.values) >= metadataLogProjectionEntries || len(text)+len(redacted) > metadataLogProjectionBytes-p.bytes {
		return
	}
	p.values[text] = redacted
	p.bytes += len(text) + len(redacted)
}

func (p *metadataLogProjection) project(text string) string {
	if redacted, ok := p.values[text]; ok {
		return redacted
	}
	redacted := logging.RedactKnown(text, p.secrets...)
	p.remember(text, redacted)
	return redacted
}

func (c metadataProjectionCache) trash(entry *TrashEntry) map[string]any {
	records := make([]map[string]any, 0, len(entry.Jobs))
	for _, job := range entry.Jobs {
		records = append(records, c.job(job))
	}
	return map[string]any{"id": entry.ID, "email": entry.Email, "deletedAt": entry.DeletedAt, "purgeStartedAt": entry.PurgeStartedAt, "state": entry.State, "lastError": entry.LastError, "jobs": records}
}
