package db

// Store is the platform's own database: every read and write of its tables.
type Store struct {
	pool *Pool
	// maxInfraRequeues bounds how many times one experiment may be returned to the queue for
	// free after an infrastructure fault, before ResolveTermination gives up and terminates it
	// with the reason that keeps ending it. Without a ceiling a job landing repeatedly on broken
	// hardware loops forever, saying nothing.
	maxInfraRequeues int
}

// NewStore creates a Store backed by the given pool. maxInfraRequeues must be positive: there is no "unbounded" and no "off" —
// a zero would silently turn every infrastructure fault back into a terminal eviction.
func NewStore(pool *Pool, maxInfraRequeues int) *Store {
	if maxInfraRequeues <= 0 {
		panic("db.NewStore: max_infrastructure_requeues must be positive")
	}
	return &Store{pool: pool, maxInfraRequeues: maxInfraRequeues}
}
