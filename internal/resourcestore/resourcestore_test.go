package resourcestore_test

import (
	"time"

	"github.com/cri-o/cri-o/internal/resourcestore"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"golang.org/x/net/context"
)

var (
	testName = "name"
	testID   = "id"
)

type entry struct {
	id      string
	created bool
}

func (e *entry) ID() string {
	return e.id
}

func (e *entry) SetCreated() {
	e.created = true
}

// The actual test suite
var _ = t.Describe("ResourceStore", func() {
	// Setup the test
	var (
		sut     *resourcestore.ResourceStore
		cleaner *resourcestore.ResourceCleaner
		e       *entry
	)
	Context("no timeout", func() {
		BeforeEach(func() {
			sut = resourcestore.New()
			cleaner = resourcestore.NewResourceCleaner()
			e = &entry{
				id: testID,
			}
		})
		AfterEach(func() {
			sut.Close()
		})
		It("Put should be able to get resource after adding", func() {
			// Given

			// When
			Expect(sut.Put(testName, e, cleaner)).To(BeNil())

			// Then
			id := sut.Get(testName)
			Expect(id).To(Equal(e.id))

			id = sut.Get(testName)
			Expect(id).To(BeEmpty())
		})
		It("Put should fail to readd resource", func() {
			// Given

			// When
			Expect(sut.Put(testName, e, cleaner)).To(BeNil())

			// Then
			Expect(sut.Put(testName, e, cleaner)).NotTo(BeNil())
		})
		It("Get should call SetCreated", func() {
			// When
			Expect(sut.Put(testName, e, cleaner)).To(BeNil())

			// Then
			id := sut.Get(testName)
			Expect(id).To(Equal(e.id))
			Expect(e.created).To(BeTrue())
		})
		It("Should not fail to Get after retrieving Watcher", func() {
			// When
			_ = sut.WatcherForResource(testName)

			// Then
			id := sut.Get(testName)
			Expect(id).To(BeEmpty())
		})
		It("Should be able to get multiple Watchers", func() {
			// Given
			watcher1 := sut.WatcherForResource(testName)
			watcher2 := sut.WatcherForResource(testName)

			waitWatcherSet := func(watcher chan struct{}) bool {
				<-watcher
				return true
			}

			// When
			Expect(sut.Put(testName, e, cleaner)).To(BeNil())
			// Then
			Expect(waitWatcherSet(watcher1)).To(BeTrue())
			Expect(waitWatcherSet(watcher2)).To(BeTrue())
		})
	})
	Context("with timeout", func() {
		BeforeEach(func() {
			cleaner = resourcestore.NewResourceCleaner()
			e = &entry{
				id: testID,
			}
		})
		AfterEach(func() {
			sut.Close()
		})
		It("Put should call cleanup funcs after timeout", func() {
			// Given
			timeout := 2 * time.Second
			sut = resourcestore.NewWithTimeout(timeout)

			timedOutChan := make(chan bool)
			cleaner.Add(context.Background(), "test", func() error {
				timedOutChan <- true
				return nil
			})
			go func() {
				time.Sleep(timeout * 3)
				timedOutChan <- false
			}()

			// When
			Expect(sut.Put(testName, e, cleaner)).To(BeNil())

			// Then
			didStoreCallTimeoutFunc := <-timedOutChan
			Expect(didStoreCallTimeoutFunc).To(Equal(true))

			id := sut.Get(testName)
			Expect(id).To(BeEmpty())
		})
		It("should not call cleanup until after resource is put", func() {
			// Given
			timeout := 2 * time.Second
			sut = resourcestore.NewWithTimeout(timeout)

			_ = sut.WatcherForResource(testName)

			timedOutChan := make(chan bool)

			// When
			go func() {
				time.Sleep(timeout * 6)
				Expect(sut.Put(testName, e, cleaner)).To(BeNil())
				timedOutChan <- true
			}()

			// Then
			didStoreWaitForPut := <-timedOutChan
			Expect(didStoreWaitForPut).To(Equal(true))
		})
	})
})
