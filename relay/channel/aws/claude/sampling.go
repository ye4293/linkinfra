package aws

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// Only remove sampling parameters explicitly rejected by Bedrock. This also
// supports new models and opaque inference profile IDs without a model allowlist.
func rejectedSamplingParameter(err error) string {
	var validationErr *types.ValidationException
	if !errors.As(err, &validationErr) {
		return ""
	}
	message := strings.ToLower(validationErr.ErrorMessage())
	message = strings.NewReplacer("`", "", "\"", "", "'", "").Replace(message)
	message = strings.TrimSuffix(strings.TrimSpace(message), ".")
	for _, parameter := range []string{"temperature", "top_p", "top_k"} {
		for _, reason := range []string{" is deprecated for this model", " is not supported for this model", " is not supported by this model"} {
			if message == parameter+reason {
				return parameter
			}
		}
	}
	return ""
}

func withSamplingFallback[T any](body []byte, invoke func([]byte) (T, error)) (T, error) {
	// At most three retries, each removing one of the three sampling parameters.
	for retries := 0; ; retries++ {
		result, err := invoke(body)
		if err == nil || retries == 3 {
			return result, err
		}
		parameter := rejectedSamplingParameter(err)
		if parameter == "" {
			return result, err
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal(body, &payload) != nil {
			return result, err
		}
		if _, exists := payload[parameter]; !exists {
			return result, err
		}
		delete(payload, parameter)
		updated, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return result, err
		}
		body = updated
	}
}

func invokeModelWithSamplingFallback(ctx context.Context, client *bedrockruntime.Client, input *bedrockruntime.InvokeModelInput) (*bedrockruntime.InvokeModelOutput, error) {
	request := *input
	return withSamplingFallback(input.Body, func(body []byte) (*bedrockruntime.InvokeModelOutput, error) {
		request.Body = body
		return client.InvokeModel(ctx, &request)
	})
}

func invokeStreamWithSamplingFallback(ctx context.Context, client *bedrockruntime.Client, input *bedrockruntime.InvokeModelWithResponseStreamInput) (*bedrockruntime.InvokeModelWithResponseStreamOutput, error) {
	request := *input
	return withSamplingFallback(input.Body, func(body []byte) (*bedrockruntime.InvokeModelWithResponseStreamOutput, error) {
		request.Body = body
		return client.InvokeModelWithResponseStream(ctx, &request)
	})
}
