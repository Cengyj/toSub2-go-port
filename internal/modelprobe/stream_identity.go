package modelprobe

import "encoding/json"

// streamIdentity binds optional progress evidence to the terminal response.
// A valid answer from another response/item is not evidence for this request.
// Missing optional progress fields remain compatible; explicit contradictory
// or malformed identity fields always fail closed.
type streamIdentity struct {
	responseID string
	model      string
	items      map[int]string
}

func bindStreamString(object map[string]json.RawMessage, key string, bound *string) error {
	if _, present := object[key]; !present {
		return nil
	}
	value, ok := stringField(object, key)
	if !ok || value == "" || *bound != "" && *bound != value {
		return invalidResponse()
	}
	*bound = value
	return nil
}

func (s *streamIdentity) observe(event map[string]json.RawMessage, requestedModel string) error {
	if err := bindStreamString(event, "response_id", &s.responseID); err != nil {
		return err
	}
	if raw, present := event["response"]; present {
		response, err := decodeObject(raw)
		if err != nil {
			return invalidResponse()
		}
		if err := bindStreamString(response, "id", &s.responseID); err != nil {
			return err
		}
		if _, present := response["model"]; present {
			model, ok := stringField(response, "model")
			if !ok || !matchingModel(requestedModel, model) {
				return &Error{Code: "model_mismatch"}
			}
			if err := bindStreamString(response, "model", &s.model); err != nil {
				return err
			}
		}
	}
	if raw, present := event["item_id"]; present {
		index, ok := indexField(event, "output_index")
		if !ok {
			return invalidResponse()
		}
		if err := s.item(index, map[string]json.RawMessage{"id": raw}); err != nil {
			return err
		}
	}
	if raw, present := event["item"]; present {
		index, ok := indexField(event, "output_index")
		if !ok {
			return invalidResponse()
		}
		item, err := decodeObject(raw)
		if err != nil {
			return invalidResponse()
		}
		if err := s.item(index, item); err != nil {
			return err
		}
	}
	return nil
}
func (s *streamIdentity) item(index int, item map[string]json.RawMessage) error {
	if index < 0 || index > 31 {
		return invalidResponse()
	}
	bound := s.items[index]
	if err := bindStreamString(item, "id", &bound); err != nil {
		return err
	}
	if bound != "" {
		if s.items == nil {
			s.items = make(map[int]string)
		}
		for other, id := range s.items {
			if other != index && id == bound {
				return invalidResponse()
			}
		}
		s.items[index] = bound
	}
	return nil
}
func (s *streamIdentity) completed(raw json.RawMessage, doneItems map[int]json.RawMessage) error {
	response, err := decodeObject(raw)
	if err != nil {
		return invalidResponse()
	}
	var output []json.RawMessage
	if json.Unmarshal(response["output"], &output) != nil {
		return invalidResponse()
	}
	if len(output) == 0 && len(doneItems) > 0 {
		for index := 0; index < len(doneItems); index++ {
			output = append(output, doneItems[index])
		}
	}
	for index := range s.items {
		if index >= len(output) {
			return invalidResponse()
		}
	}
	for index, rawItem := range output {
		item, err := decodeObject(rawItem)
		if err != nil {
			return invalidResponse()
		}
		if bound := s.items[index]; bound != "" {
			id, ok := stringField(item, "id")
			if !ok || id != bound {
				return invalidResponse()
			}
		}
		if err := s.item(index, item); err != nil {
			return err
		}
	}
	return nil
}
