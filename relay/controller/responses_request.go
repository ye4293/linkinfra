package controller

import (
	"encoding/json"
	"errors"
)

// mapResponsesModel 只替换 model，不丢弃未知字段、thinking/compaction、工具历史或显式 false/0。
func mapResponsesModel(body []byte, name string) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, errors.New("Responses request must be an object")
	}
	request["model"], _ = json.Marshal(name)
	return json.Marshal(request)
}
