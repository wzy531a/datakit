// Unless explicitly stated otherwise all files in this repository are licensed
// under the MIT License.
// This product includes software developed at Guance Cloud (https://www.guance.com/).
// Copyright 2021-present Guance, Inc.

// Package smart collects S.M.A.R.T metrics.
package smart

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GuanceCloud/cliutils"
	"github.com/GuanceCloud/cliutils/logger"
	"github.com/GuanceCloud/cliutils/point"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/command"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/datakit"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/goroutine"
	dkio "gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/io"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/metrics"
	ipath "gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/path"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/plugins/inputs"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/strarr"
)

const intelVID = "0x8086"

var (
	defSmartCmd     = "smartctl"
	defSmartCtlPath = "/usr/bin/smartctl"
	defNvmeCmd      = "nvme"
	defNvmePath     = "/usr/bin/nvme"
	defInterval     = datakit.Duration{Duration: 10 * time.Second}
	defTimeout      = datakit.Duration{Duration: 3 * time.Second}
)

var (
	inputName = "smart"
	//nolint:lll
	sampleConfig = `
[[inputs.smart]]
  ## The path to the smartctl executable
  # path_smartctl = "/usr/bin/smartctl"

  ## The path to the nvme-cli executable
  # path_nvme = "/usr/bin/nvme"

  ## Gathering interval
  # interval = "10s"

  ## Timeout for the cli command to complete.
  # timeout = "30s"

  ## Optionally specify if vendor specific attributes should be propagated for NVMe disk case
  ## ["auto-on"] - automatically find and enable additional vendor specific disk info
  ## ["vendor1", "vendor2", ...] - e.g. "Intel" enable additional Intel specific disk info
  # enable_extensions = ["auto-on"]

  ## On most platforms used cli utilities requires root access.
  ## Setting 'use_sudo' to true will make use of sudo to run smartctl or nvme-cli.
  ## Sudo must be configured to allow the telegraf user to run smartctl or nvme-cli
  ## without a password.
  # use_sudo = false

  ## Skip checking disks in this power mode. Defaults to "standby" to not wake up disks that have stopped rotating.
  ## See --nocheck in the man pages for smartctl.
  ## smartctl version 5.41 and 5.42 have faulty detection of power mode and might require changing this value to "never" depending on your disks.
  # no_check = "standby"

  ## Optionally specify devices to exclude from reporting if disks auto-discovery is performed.
  # excludes = [ "/dev/pass6" ]

  ## Optionally specify devices and device type, if unset a scan (smartctl --scan and smartctl --scan -d nvme) for S.M.A.R.T. devices will be done
  ## and all found will be included except for the excluded in excludes.
  # devices = [ "/dev/ada0 -d atacam", "/dev/nvme0"]

  ## Customer tags, if set will be seen with every metric.
  [inputs.smart.tags]
    # "key1" = "value1"
    # "key2" = "value2"
`
	l = logger.DefaultSLogger(inputName)
)

type nvmeDevice struct {
	name         string
	vendorID     string
	model        string
	serialNumber string
}

type Input struct {
	SmartCtlPath     string            `toml:"smartctl_path"`
	NvmePath         string            `toml:"nvme_path"`
	Interval         datakit.Duration  `toml:"interval"`
	Timeout          datakit.Duration  `toml:"timeout"`
	EnableExtensions []string          `toml:"enable_extensions"`
	UseSudo          bool              `toml:"use_sudo"`
	NoCheck          string            `toml:"no_check"`
	Excludes         []string          `toml:"excludes"`
	Devices          []string          `toml:"devices"`
	Tags             map[string]string `toml:"tags"`

	semStop *cliutils.Sem // start stop signal
	feeder  dkio.Feeder
	Tagger  datakit.GlobalTagger
}

func (*Input) Catalog() string {
	return inputName
}

func (*Input) SampleConfig() string {
	return sampleConfig
}

func (*Input) AvailableArchs() []string { return datakit.AllOS }

func (*Input) SampleMeasurement() []inputs.Measurement {
	return []inputs.Measurement{&smartMeasurement{}}
}

func (ipt *Input) Run() {
	l = logger.SLogger(inputName)

	var err error
	if ipt.SmartCtlPath == "" || !ipath.IsFileExists(ipt.SmartCtlPath) {
		if ipt.SmartCtlPath, err = exec.LookPath(defSmartCmd); err != nil {
			l.Error("Can not find executable sensor command, install 'smartmontools' first.")

			return
		}
		l.Infof("Command fallback to %q due to invalide path provided in 'smart' input", ipt.SmartCtlPath)
	}
	if ipt.NvmePath == "" || !ipath.IsFileExists(ipt.NvmePath) {
		if ipt.NvmePath, err = exec.LookPath(defNvmeCmd); err != nil {
			ipt.NvmePath = ""
			l.Debug("Can not find executable sensor command, install 'nvme-cli' first.")
		} else {
			l.Infof("Command fallback to %q due to invalide path provided in 'smart' input", ipt.NvmePath)
		}
	}

	l.Info("smartctl input started")

	tick := time.NewTicker(ipt.Interval.Duration)
	for {
		select {
		case <-tick.C:
			if err := ipt.gather(); err != nil {
				l.Errorf("gagher: %s", err.Error())
				metrics.FeedLastError(inputName, err.Error())
				continue
			}
		case <-datakit.Exit.Wait():
			l.Info("smart input exits")
			return

		case <-ipt.semStop.Wait():
			l.Info("smart input return")
			return
		}
	}
}

func (ipt *Input) Terminate() {
	if ipt.semStop != nil {
		ipt.semStop.Close()
	}
}

// Gather takes in an accumulator and adds the metrics that the SMART tools gather.
func (ipt *Input) gather() error {
	var (
		err                   error
		scannedNVMeDevices    []string
		scannedNonNVMeDevices []string
		isNVMe                = len(ipt.NvmePath) != 0
		isVendorExtension     = len(ipt.EnableExtensions) != 0
	)
	if len(ipt.Devices) != 0 {
		if err := ipt.getAttributes(ipt.Devices); err != nil {
			return err
		}

		// if nvme-cli is present, vendor specific attributes can be gathered
		if isVendorExtension && isNVMe {
			if scannedNVMeDevices, _, err = ipt.scanAllDevices(true); err != nil {
				return err
			}
			if err = ipt.getVendorNVMeAttributes(distinguishNVMeDevices(ipt.Devices, scannedNVMeDevices)); err != nil {
				return err
			}
		}
	} else {
		if scannedNVMeDevices, scannedNonNVMeDevices, err = ipt.scanAllDevices(false); err != nil {
			return err
		}

		var devicesFromScan []string
		devicesFromScan = append(devicesFromScan, scannedNVMeDevices...)
		devicesFromScan = append(devicesFromScan, scannedNonNVMeDevices...)
		if err := ipt.getAttributes(devicesFromScan); err != nil {
			return err
		}

		if isVendorExtension && isNVMe {
			return ipt.getVendorNVMeAttributes(scannedNVMeDevices)
		}
	}

	return nil
}

// Scan for S.M.A.R.T. devices from smartctl.
func (ipt *Input) scanDevices(ignoreExcludes bool, scanArgs ...string) ([]string, error) {
	output, err := command.RunWithTimeout(ipt.Timeout.Duration, ipt.UseSudo, ipt.SmartCtlPath, scanArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to run command '%s %s': %w - %s", ipt.SmartCtlPath, scanArgs, err, string(output))
	}

	var devices []string
	for _, line := range strings.Split(string(output), "\n") {
		dev := strings.Split(line, " ")
		if len(dev) <= 1 {
			continue
		}
		if !ignoreExcludes {
			if !excludedDevice(ipt.Excludes, strings.TrimSpace(dev[0])) {
				devices = append(devices, strings.TrimSpace(dev[0]))
			}
		} else {
			devices = append(devices, strings.TrimSpace(dev[0]))
		}
	}

	return devices, nil
}

func (ipt *Input) scanAllDevices(ignoreExcludes bool) ([]string, []string, error) {
	// this will return all devices (including NVMe devices) for smartctl version >= 7.0
	// for older versions this will return non NVMe devices
	devices, err := ipt.scanDevices(ignoreExcludes, "--scan")
	if err != nil {
		return nil, nil, err
	}

	// this will return only NVMe devices
	nvmeDevices, err := ipt.scanDevices(ignoreExcludes, "--scan", "--device=nvme")
	if err != nil {
		return nil, nil, err
	}

	// to handle all versions of smartctl this will return only non NVMe devices
	nonNVMeDevices := strarr.Differ(devices, nvmeDevices)

	return nvmeDevices, nonNVMeDevices, nil
}

func (ipt *Input) getCustomerTags() map[string]string {
	tags := make(map[string]string)
	for k, v := range ipt.Tags {
		tags[k] = v
	}

	return tags
}

// Get info and attributes for each S.M.A.R.T. device.
func (ipt *Input) getAttributes(devices []string) error {
	start := time.Now()

	g := goroutine.NewGroup(goroutine.Option{Name: "inputs_smart"})
	for _, device := range devices {
		func(device string) {
			g.Go(func(ctx context.Context) error {
				if sm, err := gatherDisk(ipt.getCustomerTags(), ipt.Timeout.Duration, ipt.UseSudo, ipt.SmartCtlPath,
					ipt.NoCheck, device); err != nil {
					l.Errorf("gatherDisk: %s", err.Error())

					metrics.FeedLastError(inputName, err.Error())
				} else {
					opts := point.DefaultMetricOptions()
					sm.tags = inputs.MergeTagsWrapper(sm.tags, ipt.Tagger.HostTags(), ipt.Tags, "")
					pt := point.NewPointV2(sm.name,
						append(point.NewTags(sm.tags), point.NewKVs(sm.fields)...), opts...)

					return ipt.feeder.FeedV2(point.Metric, []*point.Point{pt},
						dkio.WithCollectCost(time.Since(start)),
						dkio.WithInputName(inputName),
					)
				}

				return nil
			})
		}(device)
	}

	return g.Wait()
}

func (ipt *Input) getVendorNVMeAttributes(devices []string) error {
	start := time.Now()
	nvmeDevices := getDeviceInfoForNVMeDisks(devices, ipt.NvmePath, ipt.Timeout.Duration, ipt.UseSudo)

	g := goroutine.NewGroup(goroutine.Option{Name: "inputs_smart"})
	for _, device := range nvmeDevices {
		if strarr.Contains(ipt.EnableExtensions, "auto-on") {
			if device.vendorID == intelVID {
				func(device nvmeDevice) {
					g.Go(func(ctx context.Context) error {
						if sm, err := gatherIntelNVMeDisk(ipt.getCustomerTags(),
							ipt.Timeout.Duration, ipt.UseSudo, ipt.NvmePath, device); err != nil {
							l.Errorf("gatherIntelNVMeDisk: %s", err.Error())

							metrics.FeedLastError(inputName, err.Error())
						} else {
							opts := point.DefaultMetricOptions()
							sm.tags = inputs.MergeTagsWrapper(sm.tags, ipt.Tagger.HostTags(), ipt.Tags, "")
							pt := point.NewPointV2(sm.name,
								append(point.NewTags(sm.tags), point.NewKVs(sm.fields)...),
								opts...)

							return ipt.feeder.FeedV2(point.Metric, []*point.Point{pt},
								dkio.WithCollectCost(time.Since(start)),
								dkio.WithInputName(inputName),
							)
						}
						return nil
					})
				}(device)
			}
		} else if strarr.Contains(ipt.EnableExtensions, "Intel") && device.vendorID == intelVID {
			func(device nvmeDevice) {
				g.Go(func(ctx context.Context) error {
					if sm, err := gatherIntelNVMeDisk(ipt.getCustomerTags(),
						ipt.Timeout.Duration, ipt.UseSudo, ipt.NvmePath, device); err != nil {
						l.Errorf("gatherIntelNVMeDisk: %s", err.Error())
						metrics.FeedLastError(inputName, err.Error())
					} else {
						opts := point.DefaultMetricOptions()
						sm.tags = inputs.MergeTagsWrapper(sm.tags, ipt.Tagger.HostTags(), ipt.Tags, "")
						pt := point.NewPointV2(sm.name,
							append(point.NewTags(sm.tags), point.NewKVs(sm.fields)...),
							opts...)

						return ipt.feeder.FeedV2(point.Metric, []*point.Point{pt},
							dkio.WithCollectCost(time.Since(start)),
							dkio.WithInputName(inputName),
						)
					}

					return nil
				})
			}(device)
		}
	}

	return g.Wait()
}

func distinguishNVMeDevices(userDevices []string, availableNVMeDevices []string) []string {
	var nvmeDevices []string
	for _, userDevice := range userDevices {
		for _, NVMeDevice := range availableNVMeDevices {
			// double check. E.g. in case when nvme0 is equal nvme0n1, will check if "nvme0" part is present.
			if strings.Contains(NVMeDevice, userDevice) || strings.Contains(userDevice, NVMeDevice) {
				nvmeDevices = append(nvmeDevices, userDevice)
			}
		}
	}

	return nvmeDevices
}

func excludedDevice(excludes []string, deviceLine string) bool {
	device := strings.Split(deviceLine, " ")
	if len(device) != 0 {
		for _, exclude := range excludes {
			if device[0] == exclude {
				return true
			}
		}
	}

	return false
}

func gatherNVMeDeviceInfo(nvme, device string, timeout time.Duration, useSudo bool) (string, string, string, error) {
	args := append([]string{"id-ctrl"}, strings.Split(device, " ")...)
	output, err := command.RunWithTimeout(timeout, useSudo, nvme, args...)
	if err != nil {
		return "", "", "", err
	}

	return findNVMeDeviceInfo(string(output))
}

func getDeviceInfoForNVMeDisks(devices []string, nvme string, timeout time.Duration, useSudo bool) []nvmeDevice {
	var nvmeDevices []nvmeDevice
	for _, device := range devices {
		vid, sn, mn, err := gatherNVMeDeviceInfo(nvme, device, timeout, useSudo)
		if err != nil {
			l.Errorf("gatherNVMeDeviceInfo: %s", err)

			metrics.FeedLastError(inputName, fmt.Sprintf("cannot find device info for %s device", device))
			continue
		}
		newDevice := nvmeDevice{
			name:         device,
			vendorID:     vid,
			model:        mn,
			serialNumber: sn,
		}
		nvmeDevices = append(nvmeDevices, newDevice)
	}

	return nvmeDevices
}

func findNVMeDeviceInfo(output string) (string, string, string, error) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	var vid, sn, mn string

	for scanner.Scan() {
		line := scanner.Text()

		if matches := nvmeIDCtrlExpressionPattern.FindStringSubmatch(line); len(matches) > 2 {
			matches[1] = strings.TrimSpace(matches[1])
			matches[2] = strings.TrimSpace(matches[2])
			if matches[1] == "vid" {
				if _, err := fmt.Sscanf(matches[2], "%s", &vid); err != nil {
					return "", "", "", err
				}
			}
			if matches[1] == "sn" {
				sn = matches[2]
			}
			if matches[1] == "mn" {
				mn = matches[2]
			}
		}
	}

	return vid, sn, mn, nil
}

func gatherIntelNVMeDisk(tags map[string]string,
	timeout time.Duration,
	useSudo bool,
	nvme string,
	device nvmeDevice,
) (*smartMeasurement, error) {
	args := append([]string{"intel", "smart-log-add"}, strings.Split(device.name, " ")...)
	output, err := command.RunWithTimeout(timeout, useSudo, nvme, args...)
	if _, err = command.ExitStatus(err); err != nil {
		return nil, fmt.Errorf("failed to run command '%s %s': %w - %s",
			nvme, strings.Join(args, " "), err, string(output))
	}

	tags["device"] = path.Base(device.name)
	tags["model"] = device.model
	tags["serial_no"] = device.serialNumber
	fields := make(map[string]interface{})

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()

		if matches := intelExpressionPattern.FindStringSubmatch(line); len(matches) > 3 {
			matches[1] = strings.TrimSpace(matches[1])
			matches[3] = strings.TrimSpace(matches[3])
			if attr, ok := intelAttributes[matches[1]]; ok {
				parse := parseCommaSeparatedIntWithCache
				if attr.Parse != nil {
					parse = attr.Parse
				}

				if err := parse(attr.Name, fields, matches[3]); err != nil {
					continue
				}
			}
		}
	}

	return &smartMeasurement{name: "smart", tags: tags, fields: fields, ts: time.Now()}, nil
}

func parseInt(str string) int64 {
	if i, err := strconv.ParseInt(str, 10, 64); err == nil {
		return i
	}

	return 0
}

func parseRawValue(rawVal string) (int64, error) {
	// Integer
	if i, err := strconv.ParseInt(rawVal, 10, 64); err == nil {
		return i, nil
	}

	// Duration: 65h+33m+09.259s
	unit := regexp.MustCompile("^(.*)([hms])$")
	parts := strings.Split(rawVal, "+")
	if len(parts) == 0 {
		return 0, fmt.Errorf("couldn't parse RAW_VALUE '%s'", rawVal)
	}

	duration := int64(0)
	for _, part := range parts {
		timePart := unit.FindStringSubmatch(part)
		if len(timePart) == 0 {
			continue
		}
		switch timePart[2] {
		case "h":
			duration += parseInt(timePart[1]) * int64(3600)
		case "m":
			duration += parseInt(timePart[1]) * int64(60)
		case "s":
			// drop fractions of seconds
			duration += parseInt(strings.Split(timePart[1], ".")[0])
		default:
			// Unknown, ignore
		}
	}
	return duration, nil
}

func gatherDisk(tags map[string]string, timeout time.Duration, sudo bool,
	smartctl, nocheck, device string,
) (*smartMeasurement, error) {
	// smartctl 5.41 & 5.42 have are broken regarding handling of --nocheck/-n
	args := append([]string{
		"--info",
		"--health",
		"--attributes",
		"--tolerance=verypermissive",
		"-n",
		nocheck,
		"--format=brief",
	}, strings.Split(device, " ")...)
	output, err := command.RunWithTimeout(timeout, sudo, smartctl, args...)
	// Ignore all exit statuses except if it is a command line parse error
	exitStatus, err := command.ExitStatus(err)
	if err != nil {
		return nil, err
	}

	tags["device"] = path.Base(strings.Split(device, " ")[0])
	if exitStatus == 0 {
		tags["exit_status"] = "success"
	} else {
		tags["exit_status"] = "failed"
	}
	fields := make(map[string]interface{})

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()

		model := modelInfo.FindStringSubmatch(line)
		if len(model) > 2 {
			tags["model"] = model[2]
		}

		serial := serialInfo.FindStringSubmatch(line)
		if len(serial) > 1 {
			tags["serial_no"] = serial[1]
		}

		wwn := wwnInfo.FindStringSubmatch(line)
		if len(wwn) > 1 {
			tags["wwn"] = strings.ReplaceAll(wwn[1], " ", "")
		}

		capacity := userCapacityInfo.FindStringSubmatch(line)
		if len(capacity) > 1 {
			tags["capacity"] = strings.ReplaceAll(capacity[1], ",", "")
			if c, err := strconv.Atoi(tags["capacity"]); err == nil {
				c /= 1000000000
				tags["capacity"] = fmt.Sprintf("%dGB", c)
			}
		}

		enabled := smartEnabledInfo.FindStringSubmatch(line)
		if len(enabled) > 1 {
			tags["enabled"] = enabled[1]
		}

		health := smartOverallHealth.FindStringSubmatch(line)
		if len(health) > 2 {
			tags["health_ok"] = health[2]
		}

		attr := attribute.FindStringSubmatch(line)
		if len(attr) > 1 {
			// attribute has been found
			name := strings.ToLower(attr[2])
			fields["flags"] = attr[3]
			if i, err := strconv.ParseInt(attr[4], 10, 64); err == nil {
				fields[name+"_value"] = i
			}
			if i, err := strconv.ParseInt(attr[5], 10, 64); err == nil {
				fields[name+"_worst"] = i
			}
			if i, err := strconv.ParseInt(attr[6], 10, 64); err == nil {
				fields[name+"_threshold"] = i
			}
			fields["fail"] = !(attr[7] == "-")
			if val, err := parseRawValue(attr[8]); err == nil {
				fields[name+"_raw_value"] = val
			}

			// If the attribute matches on the one in deviceFieldIds save the raw value to a field.
			if field, ok := deviceFieldIds[attr[1]]; ok {
				if val, err := parseRawValue(attr[8]); err == nil {
					fields[field] = val
				}
			}
		} else if matches := sasNvmeAttr.FindStringSubmatch(line); len(matches) > 2 {
			// what was found is not a vendor attribute
			if attr, ok := sasNvmeAttributes[matches[1]]; ok {
				parse := parseCommaSeparatedInt
				if attr.Parse != nil {
					parse = attr.Parse
				}
				if err := parse(attr.Name, fields, matches[2]); err != nil {
					continue
				}
			}
		}
	}

	return &smartMeasurement{name: "smart", tags: tags, fields: fields, ts: time.Now()}, nil
}

func noinit() { //nolint:gochecknoinits
	inputs.Add(inputName, func() inputs.Input {
		return &Input{
			SmartCtlPath:     defSmartCtlPath,
			NvmePath:         defNvmePath,
			Interval:         defInterval,
			Timeout:          defTimeout,
			EnableExtensions: []string{"auto-on"},
			NoCheck:          "standby",

			semStop: cliutils.NewSem(),
			feeder:  dkio.DefaultFeeder(),
			Tagger:  datakit.DefaultGlobalTagger(),
		}
	})
}
