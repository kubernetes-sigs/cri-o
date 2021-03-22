package ctrfactory_test

import (
	"testing"

	"github.com/cri-o/cri-o/internal/ctrfactory"
	. "github.com/cri-o/cri-o/test/framework"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// TestContainerFactory runs the specs
func TestContainerFactory(t *testing.T) {
	RegisterFailHandler(Fail)
	RunFrameworkSpecs(t, "ContainerFactory")
}

var (
	t   *TestFramework
	sut *ctrfactory.ContainerFactory
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
	sut, err = ctrfactory.New()
	Expect(err).To(BeNil())
})
