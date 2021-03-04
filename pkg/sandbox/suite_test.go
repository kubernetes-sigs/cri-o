package sandbox_test

import (
	"context"
	"testing"

	"github.com/cri-o/cri-o/pkg/sandbox"
	. "github.com/cri-o/cri-o/test/framework"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// TestSandbox runs the specs
func TestSandbox(t *testing.T) {
	RegisterFailHandler(Fail)
	RunFrameworkSpecs(t, "Sandbox")
}

var (
	t   *TestFramework
	sut sandbox.Sandbox
)

var _ = BeforeSuite(func() {
	t = NewTestFramework(NilFunc, NilFunc)
	t.Setup()
})

var _ = AfterSuite(func() {
	t.Teardown()
})

var _ = BeforeEach(func() {
	var err error
	sut, err = sandbox.New(context.Background())
	Expect(err).To(BeNil())
})
