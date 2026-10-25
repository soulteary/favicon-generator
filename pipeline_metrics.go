package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metricskit "github.com/soulteary/metrics-kit/v3"
)

// These collectors share the HTTP registry, and therefore the same private
// administration endpoint and service lifetime. No URL, filename, cache key,
// signature, or error text is ever used as a metric label.
type pipelineObserver struct {
	pipeline  string
	stages    *prometheus.HistogramVec
	cache     *prometheus.CounterVec
	admission *prometheus.CounterVec
}

func newPipelineObservers(registry *metricskit.Registry) (*pipelineObserver, *pipelineObserver) {
	r := registry.WithSubsystem("pipeline")
	stages := r.Histogram("stage_duration_seconds").
		Help("Duration of individual pipeline operations, including failed operations; stages may run multiple times per request.").
		Labels("pipeline", "stage", "result").
		Buckets([]float64{0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}).
		BuildVec()
	cache := r.Counter("cache_requests_total").
		Help("HTTP cache decisions by source or transformed response; hit excludes revalidation, bypass, and errors.").
		Labels("pipeline", "layer", "result").BuildVec()
	admission := r.Counter("admission_rejected_total").
		Help("Requests rejected before pipeline work because shared admission is full.").
		Labels("pipeline", "reason").BuildVec()
	return &pipelineObserver{pipeline: "image", stages: stages, cache: cache, admission: admission},
		&pipelineObserver{pipeline: "icons", stages: stages, cache: cache, admission: admission}
}

func (o *pipelineObserver) ObserveStage(stage, result string, duration time.Duration) {
	switch stage {
	case "fetch", "decode", "transform", "encode", "pack", "write":
	default:
		return
	}
	switch result {
	case "success", "error", "canceled", "timeout", "limit":
	default:
		return
	}
	if duration < 0 {
		return
	}
	o.stages.WithLabelValues(o.pipeline, stage, result).Observe(duration.Seconds())
}

func (o *pipelineObserver) ObserveCache(layer, result string) {
	switch layer {
	case "source", "transformed":
	default:
		return
	}
	switch result {
	case "hit", "miss", "revalidated", "bypass", "error":
	default:
		return
	}
	o.cache.WithLabelValues(o.pipeline, layer, result).Inc()
}

func (o *pipelineObserver) ObserveAdmissionRejected(reason string) {
	if reason == "busy" {
		o.admission.WithLabelValues(o.pipeline, reason).Inc()
	}
}
