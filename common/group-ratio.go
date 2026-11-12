package common

import (
	"encoding/json"

	"github.com/songquanpeng/one-api/common/logger"
)

var GroupRatio = map[string]float64{
	"Start":      1,
	"Build":      1,
	"Scale":      1,
	"Enterprise": 1,
}

func GroupRatio2JSONString() string {
	jsonBytes, err := json.Marshal(GroupRatio)
	if err != nil {
		logger.SysError("error marshalling model ratio: " + err.Error())
	}
	return string(jsonBytes)
}

func UpdateGroupRatioByJSONString(jsonStr string) error {
	GroupRatio = make(map[string]float64)
	return json.Unmarshal([]byte(jsonStr), &GroupRatio)
}

func GetGroupRatio(name string) float64 {
	ratio, ok := GroupRatio[name]
	if !ok {
		logger.SysError("group ratio not found: " + name)
		return 1
	}
	return ratio
}
