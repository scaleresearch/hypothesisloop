package metricsdb

// Store reads and writes the metrics store at one GreptimeDB address.
//
// Observed terminal quota consumption is tagged with experiment_id, so each job owns its own
// series: an agent's used hours are never stored, only summed per job at read time. Writing a
// job's final cost (SetObservedBatch) is an absolute set against that job's own series, so
// replaying the same completion twice writes the same value instead of double-counting.
type Store struct {
	url string
}

// New is the store at the GreptimeDB base URL url.
func New(url string) *Store {
	return &Store{url: url}
}
