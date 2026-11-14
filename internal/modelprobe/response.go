package modelprobe

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

func invalidResponse() *Error { return &Error{Code: "invalid_stream"} }

// parseStream accepts only a fully terminated Responses stream. Individual
// deltas and [DONE] are never sufficient evidence of a completed inference.
func parseStream(body []byte, requestedModel string) (string, error) {
	return parseStreamWithPolicy(body, requestedModel, false)
}

// Native BPS omits optional response.object and message.status fields in its
// Responses contract. Only those absent fields may be inferred from a fully
// validated response.completed; explicit contradictions remain invalid.
func parseStreamWithPolicy(body []byte, requestedModel string, nativeBPS bool) (string, error) {
	return parseStreamWithPolicyAndLimit(body, requestedModel, nativeBPS, maxResponseBytes)
}

func parseStreamWithPolicyAndLimit(body []byte, requestedModel string, nativeBPS bool, responseLimit int) (string, error) {
	if responseLimit < 1 || responseLimit > pelicanMaxResponseBytes {
		return "", &Error{Code: "invalid_options"}
	}
	if len(body) > responseLimit {
		return "", &Error{Code: "response_too_large"}
	}
	if !utf8.Valid(body) {
		return "", invalidResponse()
	}
	// SSE permits one UTF-8 BOM at the start of the stream only.
	body = bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})
	var data, deltas strings.Builder
	var eventName, answer, doneText, donePart string
	var completed, ended, sawDelta, sawDoneText, sawDonePart bool
	deltaIndex := -1
	messageHint := -1
	items := make(map[int]json.RawMessage)
	declared := make(map[int]string)
	noteMessage := func(index int) error {
		if index > 31 || messageHint >= 0 && messageHint != index {
			return invalidResponse()
		}
		messageHint = index
		return nil
	}
	dispatch := func() error {
		payload := data.String()
		if payload == "[DONE]" {
			if !completed {
				return &Error{Code: "incomplete"}
			}
			if ended || eventName != "" {
				return invalidResponse()
			}
			ended = true
			return nil
		}
		if completed || ended {
			return invalidResponse()
		}
		event, err := decodeObject([]byte(payload))
		if err != nil {
			return invalidResponse()
		}
		kind, ok := stringField(event, "type")
		if !ok || eventName != "" && eventName != kind {
			return invalidResponse()
		}
		switch kind {
		case "response.failed", "error":
			return &Error{Code: "upstream_error"}
		case "response.incomplete":
			return &Error{Code: "incomplete"}
		case "response.refusal.delta", "response.refusal.done":
			return &Error{Code: "refused"}
		case "response.completed":
			text, messageIndex, err := completedAnswer(event["response"], requestedModel, items, declared, nativeBPS)
			if err != nil {
				return err
			}
			if messageHint >= 0 && messageHint != messageIndex || (sawDelta || sawDoneText) && deltaIndex != messageIndex || sawDelta && deltas.String() != text || sawDoneText && doneText != text || sawDonePart && donePart != text {
				return invalidResponse()
			}
			answer, completed = text, true
		case "response.output_item.added", "response.output_item.done":
			item, err := decodeObject(event["item"])
			if err != nil {
				return invalidResponse()
			}
			itemType, _ := stringField(item, "type")
			index, indexOK := indexField(event, "output_index")
			if !indexOK || index > 31 || declared[index] != "" && declared[index] != itemType {
				return invalidResponse()
			}
			if itemType != "message" && itemType != "reasoning" {
				return invalidResponse()
			}
			declared[index] = itemType
			if itemType == "message" {
				if err := noteMessage(index); err != nil {
					return err
				}
				if role, _ := stringField(item, "role"); role != "assistant" {
					return invalidResponse()
				}
			}
			if kind == "response.output_item.done" {
				if _, duplicate := items[index]; duplicate {
					return invalidResponse()
				}
				items[index] = event["item"]
			}
		case "response.content_part.added", "response.content_part.done":
			index, indexOK := indexField(event, "output_index")
			contentIndex, contentOK := indexField(event, "content_index")
			if !indexOK || !contentOK || contentIndex != 0 {
				return invalidResponse()
			}
			if err := noteMessage(index); err != nil {
				return err
			}
			part, err := decodeObject(event["part"])
			if err != nil {
				return invalidResponse()
			}
			partType, _ := stringField(part, "type")
			if partType == "refusal" {
				return &Error{Code: "refused"}
			}
			if partType != "output_text" {
				return invalidResponse()
			}
			if kind == "response.content_part.done" {
				text, ok := stringField(part, "text")
				if !ok || sawDonePart {
					return invalidResponse()
				}
				donePart, sawDonePart = text, true
			}
		case "response.output_text.delta", "response.output_text.done":
			index, indexOK := indexField(event, "output_index")
			contentIndex, contentOK := indexField(event, "content_index")
			if !indexOK || !contentOK || contentIndex != 0 || deltaIndex >= 0 && deltaIndex != index {
				return invalidResponse()
			}
			if err := noteMessage(index); err != nil {
				return err
			}
			deltaIndex = index
			if kind == "response.output_text.delta" {
				text, ok := stringField(event, "delta")
				if !ok || sawDoneText {
					return invalidResponse()
				}
				deltas.WriteString(text)
				sawDelta = true
			} else {
				text, ok := stringField(event, "text")
				if !ok || sawDoneText {
					return invalidResponse()
				}
				doneText, sawDoneText = text, true
			}
		case "response.created", "response.queued", "response.in_progress", "response.metadata",
			"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
			"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
			"response.reasoning_text.delta", "response.reasoning_text.done":
			// Progress, metadata and private reasoning are never used to compute
			// the score or retained in the result. Completion is required below.
		default:
			return invalidResponse()
		}
		return nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), responseLimit+1)
	scanner.Split(scanSSELines)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				if err := dispatch(); err != nil {
					return "", err
				}
			}
			data.Reset()
			eventName = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		case "event":
			if eventName != "" {
				return "", invalidResponse()
			}
			eventName = value
		case "id", "retry":
			// This client does not reconnect or replay SSE requests.
		default:
			return "", invalidResponse()
		}
	}
	if scanner.Err() != nil {
		return "", invalidResponse()
	}
	if !completed || data.Len() > 0 || eventName != "" {
		return "", &Error{Code: "incomplete"}
	}
	return answer, nil
}

// SSE recognizes LF, CRLF and CR as line endings. Wait for one more byte at
// a buffer-ending CR so a split CRLF never becomes a false empty line.
func scanSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			if i+1 < len(data) && data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
		}
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func completedAnswer(raw json.RawMessage, model string, doneItems map[int]json.RawMessage, declared map[int]string, nativeBPS bool) (string, int, error) {
	response, err := decodeObject(raw)
	if err != nil {
		return "", 0, invalidResponse()
	}
	status, _ := stringField(response, "status")
	if status == "incomplete" || status == "in_progress" || status == "queued" {
		return "", 0, &Error{Code: "incomplete"}
	}
	if status == "failed" || nonNull(response["error"]) {
		return "", 0, &Error{Code: "upstream_error"}
	}
	if nonNull(response["incomplete_details"]) {
		return "", 0, &Error{Code: "incomplete"}
	}
	id, _ := stringField(response, "id")
	object, _ := stringField(response, "object")
	_, objectPresent := response["object"]
	servedModel, _ := stringField(response, "model")
	if status != "completed" || id == "" || object != "response" && (!nativeBPS || objectPresent) || servedModel == "" {
		return "", 0, invalidResponse()
	}
	if !matchingModel(model, servedModel) {
		return "", 0, &Error{Code: "model_mismatch"}
	}
	var output []json.RawMessage
	if len(response["output"]) == 0 || response["output"][0] != '[' || json.Unmarshal(response["output"], &output) != nil {
		return "", 0, invalidResponse()
	}
	// Codex can emit completed items separately and omit them in the terminal
	// output array. Reconstruct only contiguous, explicitly completed items.
	if len(output) == 0 && len(doneItems) > 0 {
		for i := 0; i < len(doneItems); i++ {
			item, ok := doneItems[i]
			if !ok {
				return "", 0, invalidResponse()
			}
			output = append(output, item)
		}
	}
	for index := range declared {
		if index >= len(output) {
			return "", 0, invalidResponse()
		}
	}
	var answer string
	messageIndex, messages := -1, 0
	for index, rawItem := range output {
		item, err := decodeObject(rawItem)
		if err != nil {
			return "", 0, invalidResponse()
		}
		kind, _ := stringField(item, "type")
		if previous := declared[index]; previous != "" && previous != kind {
			return "", 0, invalidResponse()
		}
		var done map[string]json.RawMessage
		if rawDone, exists := doneItems[index]; exists {
			done, err = decodeObject(rawDone)
			if err != nil {
				return "", 0, invalidResponse()
			}
			finalID, hasFinalID := stringField(item, "id")
			doneID, hasDoneID := stringField(done, "id")
			if hasFinalID && hasDoneID && finalID != doneID {
				return "", 0, invalidResponse()
			}
		}
		if kind == "reasoning" {
			if itemStatus, exists := stringField(item, "status"); exists && itemStatus != "completed" {
				return "", 0, &Error{Code: "incomplete"}
			}
			continue
		}
		text, err := messageAnswer(item, nativeBPS)
		if err != nil {
			return "", 0, err
		}
		if done != nil {
			doneText, err := messageAnswer(done, nativeBPS)
			if err != nil || doneText != text {
				return "", 0, invalidResponse()
			}
		}
		messages++
		answer, messageIndex = text, index
	}
	if messages != 1 {
		return "", 0, invalidResponse()
	}
	return answer, messageIndex, nil
}

func messageAnswer(item map[string]json.RawMessage, nativeBPS bool) (string, error) {
	kind, _ := stringField(item, "type")
	role, _ := stringField(item, "role")
	status, _ := stringField(item, "status")
	_, statusPresent := item["status"]
	if kind != "message" || role != "assistant" || status != "completed" && (!nativeBPS || statusPresent) {
		return "", invalidResponse()
	}
	var content []map[string]json.RawMessage
	if json.Unmarshal(item["content"], &content) != nil || len(content) != 1 {
		return "", invalidResponse()
	}
	contentType, _ := stringField(content[0], "type")
	if contentType == "refusal" {
		return "", &Error{Code: "refused"}
	}
	text, ok := stringField(content[0], "text")
	if contentType != "output_text" || !ok || strings.TrimSpace(text) == "" {
		return "", invalidResponse()
	}
	return text, nil
}

func matchingModel(requested, reported string) bool {
	if requested == reported {
		return true
	}
	suffix, ok := strings.CutPrefix(reported, requested+"-")
	if !ok {
		return false
	}
	_, err := time.Parse(time.DateOnly, suffix)
	return err == nil
}

func nonNull(raw json.RawMessage) bool {
	return len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func stringField(object map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := object[key]
	var value string
	if !ok || len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func indexField(object map[string]json.RawMessage, key string) (int, bool) {
	raw, exists := object[key]
	var index int
	if !exists || json.Unmarshal(raw, &index) != nil || index < 0 {
		return 0, false
	}
	return index, true
}

var errInvalidJSON = errors.New("invalid or ambiguous JSON")

// decodeObject rejects duplicate keys at every nesting level and additional
// JSON values. encoding/json alone would silently accept the last duplicate.
func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, errInvalidJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := uniqueValue(decoder, 0); err != nil {
		return nil, errInvalidJSON
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errInvalidJSON
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return nil, errInvalidJSON
	}
	return object, nil
}

func uniqueValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errInvalidJSON
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		keys := map[string]bool{}
		for decoder.More() {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok || keys[key] {
				return errInvalidJSON
			}
			keys[key] = true
			if err := uniqueValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errInvalidJSON
		}
	case '[':
		for decoder.More() {
			if err := uniqueValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errInvalidJSON
		}
	default:
		return errInvalidJSON
	}
	return nil
}
