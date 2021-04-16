package resourcestore

import (
	"context"
	"time"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/cri-o/cri-o/internal/log"
)

// ResourceCleaner is a structure that tracks
// how to cleanup a resource.
// CleanupFuncs can be added to it, and it can be told to
// Cleanup the resource
type ResourceCleaner struct {
	funcs []cleanupFunc
}

// A cleanupFunc is a function that cleans up one piece of
// the associated resource.
type cleanupFunc func() error

// NewResourceCleaner creates a new ResourceCleaner
func NewResourceCleaner() *ResourceCleaner {
	return &ResourceCleaner{}
}

// Add adds a new CleanupFunc to the ResourceCleaner
func (r *ResourceCleaner) Add(
	ctx context.Context,
	description string,
	fn func() error,
) {
	// Create a retry task on top of the provided function
	task := func() error {
		err := retry(ctx, fn)
		if err != nil {
			log.Errorf(ctx,
				"Retried cleanup function %q too often, giving up",
				description,
			)
		}
		return err
	}

	// Prepend reverse iterate by default
	r.funcs = append([]cleanupFunc{task}, r.funcs...)
}

// Cleanup cleans up the resource, running
// the cleanup funcs in opposite chronological order
func (r *ResourceCleaner) Cleanup() error {
	for _, f := range r.funcs {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

// retry attempts to execute fn up to defaultRetryTimes if its failure meets
// retryCondition.
func retry(ctx context.Context, fn func() error) error {
	backoff := wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   1.5,
		Steps:    defaultRetryTimes,
	}

	waitErr := wait.ExponentialBackoff(backoff, func() (bool, error) {
		if err := fn(); err != nil {
			log.Errorf(ctx, "Failed to cleanup (probably retrying): %v", err)
			return false, nil
		}
		return true, nil
	})

	if waitErr != nil {
		return errors.Wrap(waitErr, "wait on retry")
	}

	return nil
}
