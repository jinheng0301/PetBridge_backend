package llm

// PriceMicros returns the price per million tokens in micro-USD (1e-6 USD).
// ok is false for an unknown model.
func PriceMicros(model string) (inPerM, outPerM int64, ok bool) {
	switch model {
	case "claude-haiku-5-5":
		return 100000, 500000, true // $0.10 / $0.50 per M tokens
	}
	return 0, 0, false
}
