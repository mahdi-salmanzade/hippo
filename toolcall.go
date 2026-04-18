package hippo

import "encoding/json"

// UnmarshalArgs decodes a ToolCall's raw JSON arguments into T.
//
// ToolCall.Arguments is kept as json.RawMessage so providers can pass
// the model's tool-argument payload through without re-serialising.
// Callers that want structured access use this helper:
//
//	type Args struct { City string `json:"city"` }
//	args, err := hippo.UnmarshalArgs[Args](tc)
//
// The zero value of T is returned on error.
func UnmarshalArgs[T any](tc ToolCall) (T, error) {
	var v T
	if err := json.Unmarshal(tc.Arguments, &v); err != nil {
		var zero T
		return zero, err
	}
	return v, nil
}
