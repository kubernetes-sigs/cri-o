package rdt

import (
	"io/ioutil"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func tempFileWithData(data string) string {
	f := t.MustTempFile("")
	Expect(ioutil.WriteFile(f, []byte(data), 0o644)).To(BeNil())
	return f
}

// The actual test suite
var _ = t.Describe("When parsing RDT config file", func() {
	t.Describe("non-existent file", func() {
		It("should return an error", func() {
			_, err := loadConfigFile("non-existent-file")
			Expect(err).NotTo(BeNil())
		})
	})

	t.Describe("invalid file format", func() {
		It("should return an error", func() {
			f := tempFileWithData(`partitions:
- foo
`)
			_, err := loadConfigFile(f)
			Expect(err).NotTo(BeNil())
		})
	})

	t.Describe("correct file format", func() {
		It("should not return an error", func() {
			f := tempFileWithData(`partitions:
  default:
    l3Allocation: 100%
    classes:
      default:
`)
			_, err := loadConfigFile(f)
			Expect(err).To(BeNil())
		})
	})
})
