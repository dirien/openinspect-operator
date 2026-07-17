package controller

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// readyGauge is 1 while a CR's Ready condition is True; the series is
	// removed when the CR is deleted.
	readyGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "openinspect_ready",
		Help: "Whether the OpenInspect resource's Ready condition is True.",
	}, []string{"namespace", "name"})

	// ensureDuration times each area of the reconcile pass, complementing
	// controller-runtime's whole-reconcile histogram.
	ensureDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "openinspect_ensure_duration_seconds",
		Help: "Duration of one ensure area (d1, storage, workers, web) within a reconcile.",
	}, []string{"area"})
)

func init() {
	metrics.Registry.MustRegister(readyGauge, ensureDuration)
}

// timed runs fn and records its duration under the area label.
func timed(area string, fn func() error) error {
	start := time.Now()
	err := fn()
	ensureDuration.WithLabelValues(area).Observe(time.Since(start).Seconds())
	return err
}
