package metrics_test

import (
	"testing"
	"time"

	"github.com/cri-o/cri-o/pkg/config"
	"github.com/cri-o/cri-o/server/metrics"
	"github.com/cri-o/cri-o/server/metrics/collectors"
	. "github.com/cri-o/cri-o/test/framework"
	metricsmock "github.com/cri-o/cri-o/test/mocks/metrics"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pkg/errors"
)

// TestMetrics runs the created specs
func TestMetrics(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Metrics")
}

// nolint: gochecknoglobals
var t *TestFramework

var _ = BeforeSuite(func() {
	t = NewTestFramework(NilFunc, NilFunc)
	t.Setup()
})

var _ = AfterSuite(func() {
	t.Teardown()
})

// The actual test suite
var _ = t.Describe("Metrics", func() {
	var (
		mockCtrl *gomock.Controller
		mock     *metricsmock.MockImpl
		cfg      *config.MetricsConfig
		stop     chan struct{}
		errTest  = errors.New("error")
	)

	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		mock = metricsmock.NewMockImpl(mockCtrl)
		cfg = &config.MetricsConfig{
			EnableMetrics:     true,
			MetricsCollectors: collectors.All(),
		}
		stop = make(chan struct{})
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	newSut := func() *metrics.Metrics {
		sut := metrics.New(cfg)
		sut.SetImpl(mock)
		return sut
	}

	It("should succeed to start default metrics server", func() {
		// Given
		sut := newSut()
		gomock.InOrder(
			mock.EXPECT().Register(gomock.Any()).AnyTimes().Return(nil),
			mock.EXPECT().Listen(gomock.Any(), gomock.Any()).Return(nil, nil),
			mock.EXPECT().Serve(gomock.Any(), gomock.Any()).Return(nil),
		)

		// When
		err := sut.Start(stop)
		sut.Wait()

		// Then
		Expect(err).To(BeNil())
	})

	It("should succeed to start metrics server with custom port", func() {
		// Given
		cfg.MetricsPort = 30030
		sut := newSut()
		gomock.InOrder(
			mock.EXPECT().Register(gomock.Any()).AnyTimes().Return(nil),
			mock.EXPECT().Listen(gomock.Any(), gomock.Any()).Return(nil, nil),
			mock.EXPECT().Serve(gomock.Any(), gomock.Any()).Return(nil),
		)

		// When
		err := sut.Start(stop)
		sut.Wait()

		// Then
		Expect(err).To(BeNil())
	})

	It("should succeed to start metrics server with socket", func() {
		// Given
		cfg.MetricsSocket = t.MustTempFile("test-metrics-")
		sut := newSut()
		gomock.InOrder(
			mock.EXPECT().Register(gomock.Any()).AnyTimes().Return(nil),
			mock.EXPECT().Listen(gomock.Any(), gomock.Any()).Return(nil, nil),
			mock.EXPECT().Serve(gomock.Any(), gomock.Any()).AnyTimes().Return(nil),
			mock.EXPECT().RemoveUnusedSocket(gomock.Any()).AnyTimes().Return(nil),
			mock.EXPECT().Listen(gomock.Any(), gomock.Any()).Return(nil, nil),
			mock.EXPECT().Serve(gomock.Any(), gomock.Any()).AnyTimes().Return(nil),
		)

		// When
		err := sut.Start(stop)
		sut.Wait()
		sut.Wait()

		// Then
		Expect(err).To(BeNil())
	})

	It("should fail if metrics register errors", func() {
		// Given
		sut := newSut()
		gomock.InOrder(
			mock.EXPECT().Register(gomock.Any()).Return(errTest),
		)

		// When
		err := sut.Start(stop)

		// Then
		Expect(err).NotTo(BeNil())
	})

	It("should fail if listen errors", func() {
		// Given
		sut := newSut()
		gomock.InOrder(
			mock.EXPECT().Register(gomock.Any()).AnyTimes().Return(nil),
			mock.EXPECT().Listen(gomock.Any(), gomock.Any()).Return(nil, errTest),
		)

		// When
		err := sut.Start(stop)

		// Then
		Expect(err).NotTo(BeNil())
	})

	t.Describe("SinceInMicroseconds", func() {
		It("should succeed", func() {
			// Given
			// When
			res := metrics.SinceInMicroseconds(
				time.Now().Add(-time.Millisecond))

			// Then
			Expect(res).NotTo(BeZero())
		})

		It("should be zero at time.Now()", func() {
			// Given
			// When
			res := metrics.SinceInMicroseconds(time.Now())

			// Then
			Expect(res).To(BeZero())
		})
	})
})
