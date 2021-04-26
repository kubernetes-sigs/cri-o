package resourcestore_test

import (
	"errors"

	"github.com/cri-o/cri-o/internal/resourcestore"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"golang.org/x/net/context"
)

// The actual test suite
var _ = t.Describe("ResourceCleaner", func() {
	It("should call the cleanup functions", func() {
		// Given
		sut := resourcestore.NewResourceCleaner()
		called1 := false
		called2 := false
		sut.Add(context.Background(), "test1", func() error {
			called1 = true
			return nil
		})
		sut.Add(context.Background(), "test2", func() error {
			called2 = true
			return nil
		})

		// When
		err := sut.Cleanup()

		// Then
		Expect(err).To(BeNil())
		Expect(called1).To(BeTrue())
		Expect(called2).To(BeTrue())
	})

	It("should retry the cleanup functions", func() {
		// Given
		sut := resourcestore.NewResourceCleaner()
		called1 := false
		called2 := false
		sut.Add(context.Background(), "test1", func() error {
			called1 = true
			return nil
		})
		failureCnt := 0
		sut.Add(context.Background(), "test2", func() error {
			if failureCnt == 2 {
				called2 = true
				return nil
			}
			failureCnt++
			return errors.New("")
		})

		// When
		err := sut.Cleanup()

		// Then
		Expect(err).To(BeNil())
		Expect(called1).To(BeTrue())
		Expect(called2).To(BeTrue())
		Expect(failureCnt).To(Equal(2))
	})

	It("should retry three times", func() {
		// Given
		sut := resourcestore.NewResourceCleaner()
		failureCnt := 0
		sut.Add(context.Background(), "test", func() error {
			failureCnt++
			return errors.New("")
		})

		// When
		err := sut.Cleanup()

		// Then
		Expect(err).NotTo(BeNil())
		Expect(failureCnt).To(Equal(3))
	})

	It("should run in parallel", func() {
		// Given
		sut := resourcestore.NewResourceCleaner()
		testChan := make(chan bool, 1)
		succ := false
		sut.Add(context.Background(), "test1", func() error {
			testChan <- true
			return nil
		})
		sut.Add(context.Background(), "test2", func() error {
			<-testChan
			succ = true
			return nil
		})

		// When
		err := sut.Cleanup()

		// Then
		Expect(err).To(BeNil())
		Expect(succ).To(BeTrue())
	})
})
