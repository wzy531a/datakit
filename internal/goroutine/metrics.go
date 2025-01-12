// Unless explicitly stated otherwise all files in this repository are licensed
// under the MIT License.
// This product includes software developed at Guance Cloud (https://www.guance.com/).
// Copyright 2021-present Guance, Inc.

package goroutine

import (
	"github.com/GuanceCloud/cliutils/metrics"
	p8s "github.com/prometheus/client_golang/prometheus"
)

var (
	goroutineGroups  p8s.Gauge
	goroutineCostVec *p8s.SummaryVec

	goroutineStoppedVec,
	goroutineRecoverVec,
	goroutineCrashedVec *p8s.CounterVec

	goroutineCounterVec *p8s.GaugeVec
)

func metricsSetup() {
	goroutineCounterVec = p8s.NewGaugeVec(
		p8s.GaugeOpts{
			Namespace: "datakit",
			Subsystem: "goroutine",
			Name:      "alive",
			Help:      "Alive Goroutine count",
		},
		[]string{
			"name",
		},
	)

	goroutineRecoverVec = p8s.NewCounterVec(
		p8s.CounterOpts{
			Namespace: "datakit",
			Subsystem: "goroutine",
			Name:      "recover_total",
			Help:      "Recovered Goroutine count",
		},
		[]string{
			"name",
		},
	)

	goroutineStoppedVec = p8s.NewCounterVec(
		p8s.CounterOpts{
			Namespace: "datakit",
			Subsystem: "goroutine",
			Name:      "stopped_total",
			Help:      "Stopped Goroutine count",
		},
		[]string{
			"name",
		},
	)

	goroutineCrashedVec = p8s.NewCounterVec(
		p8s.CounterOpts{
			Namespace: "datakit",
			Subsystem: "goroutine",
			Name:      "crashed_total",
			Help:      "Crashed goroutines count",
		},
		[]string{
			"name",
		},
	)

	goroutineGroups = p8s.NewGauge(
		p8s.GaugeOpts{
			Namespace: "datakit",
			Subsystem: "goroutine",
			Name:      "groups",
			Help:      "Goroutine group count",
		},
	)

	goroutineCostVec = p8s.NewSummaryVec(
		p8s.SummaryOpts{
			Namespace: "datakit",
			Subsystem: "goroutine",
			Name:      "cost_seconds",
			Help:      "Goroutine running duration",

			Objectives: map[float64]float64{
				0.5:  0.05,
				0.9:  0.01,
				0.99: 0.001,
			},
		},
		[]string{
			"name",
		},
	)

	metrics.MustRegister(
		goroutineGroups,
		goroutineCostVec,
		goroutineCounterVec,
		goroutineStoppedVec,
		goroutineRecoverVec,
		goroutineCrashedVec,
	)
}

//nolint:gochecknoinits
func noinit() {
	metricsSetup()
}
