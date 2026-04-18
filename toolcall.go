package hippo

// UnmarshalArgs decodes a ToolCall's raw JSON arguments into T.
//
// ToolCall.Arguments is intentionally kept as []byte so providers can
// pass the model's tool-argument payload through without re-serialising.
// Callers that want structured access use this helper:
//
//	type Args struct { City string `json:"city"` }
//	args, err := hippo.UnmarshalArgs[Args](tc)
//
// The zero value of T is returned on error.
func UnmarshalArgs[T any](tc ToolCall) (T, error) {
	var zero T
	_ = tc
	// TODO: return json.Unmarshal(tc.Arguments, &v).
	return zero, ErrNotImplemented
}
