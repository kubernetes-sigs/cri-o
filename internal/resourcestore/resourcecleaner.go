package resourcestore

// A CleanupFunc is a function that cleans up one piece of
// the associated resource.
type CleanupFunc func()

// ResourceCleaner is a structure that tracks
// how to cleanup a resource.
// CleanupFuncs can be added to it, and it can be told to
// Cleanup the resource
type ResourceCleaner struct {
	funcs []CleanupFunc
}

// NewResourceCleaner creates a new ResourceCleaner
func NewResourceCleaner() *ResourceCleaner {
	return &ResourceCleaner{
		funcs: make([]CleanupFunc, 0),
	}
}

// Add adds a new CleanupFunc to the ResourceCleaner
func (r *ResourceCleaner) Add(f func()) {
	r.funcs = append(r.funcs, CleanupFunc(f))
}

// Cleanup cleans up the resource, running
// the cleanup funcs in opposite chronological order
func (r *ResourceCleaner) Cleanup() {
	for i := len(r.funcs) - 1; i >= 0; i-- {
		r.funcs[i]()
	}
}
