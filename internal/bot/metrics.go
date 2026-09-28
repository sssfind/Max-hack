package bot

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	productFunnel = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "skillgap",
		Subsystem: "product",
		Name:      "funnel_events_total",
		Help:      "Non-PII product funnel events.",
	}, []string{"step"})
	analysisResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "skillgap",
		Subsystem: "analysis",
		Name:      "results_total",
		Help:      "Analysis outcomes by result and data mode.",
	}, []string{"result", "mode"})
	analysisLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "skillgap",
		Subsystem: "analysis",
		Name:      "duration_seconds",
		Help:      "End-to-end vacancy analysis duration.",
		Buckets:   []float64{1, 3, 5, 10, 20, 40, 60, 90},
	}, []string{"result"})
	feedbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "skillgap",
		Subsystem: "product",
		Name:      "feedback_total",
		Help:      "Anonymous result usefulness feedback.",
	}, []string{"rating"})
)

func init() {
	prometheus.MustRegister(productFunnel, analysisResults, analysisLatency, feedbackTotal)
}

func trackFunnel(step string) {
	productFunnel.WithLabelValues(step).Inc()
}

func trackAnalysis(result, mode string, started time.Time) {
	if mode == "" {
		mode = "unknown"
	}
	analysisResults.WithLabelValues(result, mode).Inc()
	if !started.IsZero() {
		analysisLatency.WithLabelValues(result).Observe(time.Since(started).Seconds())
	}
}
