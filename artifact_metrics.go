package main

import (
	"github.com/prometheus/client_golang/prometheus"
	metricskit "github.com/soulteary/metrics-kit/v3"
)

type artifactObserver struct{ requests *prometheus.CounterVec }

func newArtifactObserver(registry *metricskit.Registry) *artifactObserver {
	return &artifactObserver{requests: registry.WithSubsystem("artifact").Counter("cache_requests_total").
		Help("Finished artifact cache decisions after source authorization and validation.").
		Labels("backend", "result").BuildVec()}
}

func (o *artifactObserver) Observe(backend, result string) {
	switch backend {
	case "off", "memory", "redis", "tiered":
	default:
		return
	}
	switch result {
	case "hit", "miss", "bypass", "error":
	default:
		return
	}
	o.requests.WithLabelValues(backend, result).Inc()
}
