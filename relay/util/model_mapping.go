package util

import (
	"encoding/json"
	"fmt"
)

// RewriteRequestModel replaces only the top-level model, preserving native API
// extension fields and numeric precision. The original body is kept for retries.
func RewriteRequestModel(body []byte, modelName string) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	modelJSON, err := json.Marshal(modelName)
	if err != nil {
		return nil, err
	}
	request["model"] = modelJSON
	return json.Marshal(request)
}

func GetMappedModelName(modelName string, mapping map[string]string) (string, bool) {
	if mapping == nil {
		return modelName, false
	}
	mappedModelName := mapping[modelName]
	if mappedModelName != "" {
		return mappedModelName, true
	}
	return modelName, false
}
