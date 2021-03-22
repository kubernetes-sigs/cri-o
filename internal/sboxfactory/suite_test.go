package sboxfactory_test

import (
	"testing"

	"github.com/cri-o/cri-o/internal/sboxfactory"
	. "github.com/cri-o/cri-o/test/framework"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// TestSandboxFactory runs the specs
func TestSandboxFactory(t *testing.T) {
	RegisterFailHandler(Fail)
	RunFrameworkSpecs(t, "SandboxFactory")
}

var (
	t   *TestFramework
	sut *sboxfactory.SandboxFactory
)

var _ = BeforeSuite(func() {
	t = NewTestFramework(NilFunc, NilFunc)
	t.Setup()
})

var _ = AfterSuite(func() {
	t.Teardown()
})

var _ = BeforeEach(func() {
	sut = sboxfactory.New()
})
