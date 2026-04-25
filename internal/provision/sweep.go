package provision

// Sweeper is the interface every resource implements so TestMain can sweep
// leaked `it-*` leftovers across every resource type without the sweep
// caller knowing the specifics.
type Sweeper interface {
	// Name returns a short human label for logs ("applications", "carriers").
	Name() string
	// Sweep deletes all it-<otherRunID>-* resources. Returns the count
	// deleted (not counting errors) and logs per-resource errors to t/stdout
	// at the caller's discretion.
	Sweep(protectRunID string) (deleted int, err error)
}
