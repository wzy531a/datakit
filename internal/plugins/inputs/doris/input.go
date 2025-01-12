// Unless explicitly stated otherwise all files in this repository are licensed
// under the MIT License.
// This product includes software developed at Guance Cloud (https://www.guance.com/).
// Copyright 2021-present Guance, Inc.

// Package doris collect doris metrics by using input prom
//
//nolint:lll
package doris

import (
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/datakit"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/plugins/inputs"
)

const (
	inputName   = "doris"
	catalogName = "db"
)

type Input struct{}

var _ inputs.InputV2 = (*Input)(nil)

func (*Input) Run() { /*nil*/ }

func (*Input) Catalog() string { return catalogName }

func (*Input) Terminate() { /*do nothing*/ }

func (*Input) SampleConfig() string { return sampleCfg }

func (*Input) AvailableArchs() []string { return datakit.AllOSWithElection }

func (*Input) SampleMeasurement() []inputs.Measurement {
	return []inputs.Measurement{
		&feMeasurement{},
		&beMeasurement{},
		&commonMeasurement{},
		&jvmMeasurement{},
	}
}

func noinit() { //nolint:gochecknoinits
	inputs.Add(inputName, func() inputs.Input {
		return &Input{}
	})
}
