package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	// CRIOOperationsKey is the key for CRI-O operation metrics.
	CRIOOperationsKey = "crio_operations"

	// CRIOOperationsLatencyKey is the key for the operation latency metrics.
	CRIOOperationsLatencyKey = "crio_operations_latency_microseconds"

	// CRIOOperationsErrorsKey is the key for the operation error metrics.
	CRIOOperationsErrorsKey = "crio_operations_errors"

	// CRIOImagePullsByDigestKey is the key for CRI-O image pull metrics by digest.
	CRIOImagePullsByDigestKey = "crio_image_pulls_by_digest"

	// CRIOImagePullsByNameKey is the key for CRI-O image pull metrics by name.
	CRIOImagePullsByNameKey = "crio_image_pulls_by_name"

	// CRIOImagePullsByNameSkippedKey is the key for CRI-O skipped image pull metrics by name (skipped).
	CRIOImagePullsByNameSkippedKey = "crio_image_pulls_by_name_skipped"

	// CRIOImagePullsFailuresKey is the key for failed image downloads in CRI-O.
	CRIOImagePullsFailuresKey = "crio_image_pulls_failures"

	// CRIOImagePullsSuccessesKey is the key for successful image downloads in CRI-O.
	CRIOImagePullsSuccessesKey = "crio_image_pulls_successes"

	// CRIOImageLayerReuseKey is the key for the CRI-O image layer reuse metrics.
	CRIOImageLayerReuseKey = "crio_image_layer_reuse"

	subsystem = "container_runtime"
)

var (
	// CRIOOperations collects operation counts by operation type.
	CRIOOperations = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      CRIOOperationsKey,
			Help:      "Cumulative number of CRI-O operations by operation type.",
		},
		[]string{"operation_type"},
	)

	// CRIOOperationsLatency collects operation latency numbers by operation
	// type.
	CRIOOperationsLatency = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Subsystem: subsystem,
			Name:      CRIOOperationsLatencyKey,
			Help:      "Latency in microseconds of CRI-O operations. Broken down by operation type.",
		},
		[]string{"operation_type"},
	)

	// CRIOOperationsErrors collects operation errors by operation
	// type.
	CRIOOperationsErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      CRIOOperationsErrorsKey,
			Help:      "Cumulative number of CRI-O operation errors by operation type.",
		},
		[]string{"operation_type"},
	)

	// CRIOImagePullsByDigest collects image pull metrics for every image digest
	CRIOImagePullsByDigest = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      CRIOImagePullsByDigestKey,
			Help:      "Bytes transferred by CRI-O image pulls by digest",
		},
		[]string{"name", "digest", "mediatype", "size"},
	)

	// CRIOImagePullsByName collects image pull metrics for every image name
	CRIOImagePullsByName = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      CRIOImagePullsByNameKey,
			Help:      "Bytes transferred by CRI-O image pulls by name",
		},
		[]string{"name", "size"},
	)

	// CRIOImagePullsByNameSkipped collects image pull metrics for every image name (skipped)
	CRIOImagePullsByNameSkipped = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      CRIOImagePullsByNameSkippedKey,
			Help:      "Bytes skipped by CRI-O image pulls by name",
		},
		[]string{"name"},
	)

	// CRIOImagePullsFailures collects image pull failures
	CRIOImagePullsFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      CRIOImagePullsFailuresKey,
			Help:      "Cumulative number of CRI-O image pull failures by error.",
		},
		[]string{"name", "error"},
	)

	// CRIOImagePullsSuccesses collects image pull successes
	CRIOImagePullsSuccesses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      CRIOImagePullsSuccessesKey,
			Help:      "Cumulative number of CRI-O image pull successes.",
		},
		[]string{"name"},
	)

	// CRIOImageLayerReuse collects image pull metrics for every resused image layer
	CRIOImageLayerReuse = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: subsystem,
			Name:      CRIOImageLayerReuseKey,
			Help:      "Reused (not pulled) local image layer count by name",
		},
		[]string{"name"},
	)
)

var registerMetrics sync.Once

// Register all metrics
func Register() {
	registerMetrics.Do(func() {
		prometheus.MustRegister(CRIOOperations)
		prometheus.MustRegister(CRIOOperationsLatency)
		prometheus.MustRegister(CRIOOperationsErrors)
		prometheus.MustRegister(CRIOImagePullsByDigest)
		prometheus.MustRegister(CRIOImagePullsByName)
		prometheus.MustRegister(CRIOImagePullsByNameSkipped)
		prometheus.MustRegister(CRIOImagePullsFailures)
		prometheus.MustRegister(CRIOImagePullsSuccesses)
		prometheus.MustRegister(CRIOImageLayerReuse)
	})
}

// SinceInMicroseconds gets the time since the specified start in microseconds.
func SinceInMicroseconds(start time.Time) float64 {
	return float64(time.Since(start).Nanoseconds() / time.Microsecond.Nanoseconds())
}
