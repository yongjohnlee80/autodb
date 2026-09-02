package exec

// countForTest is the number of live sessions in the registry — test-only.
func (r *sessionRegistry) countForTest() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}
