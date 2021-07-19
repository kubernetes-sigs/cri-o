// +build test
// All *_inject.go files are meant to be used by tests only. Purpose of this
// files is to provide a way to inject mocked data into the current setup.

package resourcestore

// defaultRetryTimes reduces the amount of default retries for testing
// purposes.
var defaultRetryTimes = 3
