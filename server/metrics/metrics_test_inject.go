// +build test
// All *_inject.go files are meant to be used by tests only. Purpose of this
// files is to provide a way to inject mocked data into the current setup.

package metrics

// SetImpl can be used to set the internal metrics implementation.
func (m *Metrics) SetImpl(impl Impl) {
	m.impl = impl
}

// Wait can be used to wait for the serving goroutine to be finished.
func (m *Metrics) Wait() {
	<-m.finished
}
