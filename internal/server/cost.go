package server

// CostRate is a per-model price in USD per one million tokens. Local models
// normally leave both at 0 (free); a rate is meaningful when Mainspring adopts a
// metered API backend, or to model opportunity cost for routing.
type CostRate struct {
	inPerMTok  float64
	outPerMTok float64
}

// SetCostRates configures per-model USD pricing (keyed by resolved model id).
// Models absent from the map cost 0. Safe to call at startup.
func (s *Server) SetCostRates(rates map[string]CostRate) { s.costRates = rates }

// NewCostRate builds a CostRate from USD-per-million-token input/output prices.
// Exported so main can construct the map from config without leaking the field
// layout.
func NewCostRate(inPerMTok, outPerMTok float64) CostRate {
	return CostRate{inPerMTok: inPerMTok, outPerMTok: outPerMTok}
}

// costFor returns the USD cost of a request given its exact token usage. It is
// only meaningful for exact usage: with estimate-only counts the prompt side is
// unknown, so callers pass exact=false and get 0 — cost is never guessed.
func (s *Server) costFor(model string, prompt, completion int64, exact bool) float64 {
	if !exact || s.costRates == nil {
		return 0
	}
	r, ok := s.costRates[model]
	if !ok {
		return 0
	}
	return float64(prompt)/1e6*r.inPerMTok + float64(completion)/1e6*r.outPerMTok
}
