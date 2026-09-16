package bot

import "expvar"

// Counters exposed through expvar (/debug/vars, dev only). A handful of
// invited users does not warrant a metrics stack, but silent drops must
// still be observable: a rising bot_dropped_total is how an
// unregistered number, a stale WhatsApp lid or an unexpected addressing
// mode shows itself.
var (
	droppedTotal   = expvar.NewInt("bot_dropped_total")
	processedTotal = expvar.NewInt("bot_processed_total")
	errorsTotal    = expvar.NewInt("bot_errors_total")
)

// DroppedTotal reports messages refused by the sender gate.
func DroppedTotal() int64 { return droppedTotal.Value() }

// ProcessedTotal reports messages that produced a reply.
func ProcessedTotal() int64 { return processedTotal.Value() }

// ErrorsTotal reports messages that failed mid-processing.
func ErrorsTotal() int64 { return errorsTotal.Value() }
