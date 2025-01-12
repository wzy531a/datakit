// Unless explicitly stated otherwise all files in this repository are licensed
// under the MIT License.
// This product includes software developed at Guance Cloud (https://www.guance.com/).
// Copyright 2021-present Guance, Inc.

// Package disk collect host disk metrics.
package disk

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/GuanceCloud/cliutils"
	"github.com/GuanceCloud/cliutils/logger"
	"github.com/GuanceCloud/cliutils/point"

	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/config"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/datakit"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/export/doc"
	dkio "gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/io"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/metrics"
	"gitlab.jiagouyun.com/cloudcare-tools/datakit/internal/plugins/inputs"
)

const (
	minInterval = time.Second
	maxInterval = time.Minute
	inputName   = "disk"
	metricName  = inputName
)

var (
	_ inputs.ReadEnv   = (*Input)(nil)
	_ inputs.Singleton = (*Input)(nil)
	l                  = logger.DefaultSLogger(inputName)
)

type DiskCacheEntry struct {
	Disks       []string
	LastUpdated time.Time
}

type Input struct {
	Interval time.Duration

	Tags          map[string]string `toml:"tags"`
	ExtraDevice   []string          `toml:"extra_device"`
	ExcludeDevice []string          `toml:"exclude_device"`

	IgnoreZeroBytesDisk bool `toml:"ignore_zero_bytes_disk"`
	OnlyPhysicalDevice  bool `toml:"only_physical_device"`
	EnableLVMMapperPath bool `toml:"enable_lvm_mapper_path"`
	MergeOnDevice       bool `toml:"merge_on_device"`

	semStop      *cliutils.Sem
	collectCache []*point.Point
	diskStats    PSDiskStats
	feeder       dkio.Feeder
	mergedTags   map[string]string
	tagger       datakit.GlobalTagger

	diskCache map[string]DiskCacheEntry
	hostRoot  string
}

func (ipt *Input) Run() {
	ipt.setup()

	ipt.ExtraDevice = unique(ipt.ExtraDevice)
	ipt.ExcludeDevice = unique(ipt.ExcludeDevice)

	tick := time.NewTicker(ipt.Interval)
	defer tick.Stop()

	start := time.Now()
	for {
		if err := ipt.collect(start.UnixNano()); err != nil {
			l.Errorf("collect: %s", err)
			ipt.feeder.FeedLastError(err.Error(),
				metrics.WithLastErrorInput(inputName),
				metrics.WithLastErrorCategory(point.Metric),
			)
		}

		if len(ipt.collectCache) > 0 {
			if err := ipt.feeder.FeedV2(point.Metric, ipt.collectCache,
				dkio.WithCollectCost(time.Since(start)),
				dkio.WithElection(false),
				dkio.WithInputName(metricName)); err != nil {
				ipt.feeder.FeedLastError(err.Error(),
					metrics.WithLastErrorInput(inputName),
					metrics.WithLastErrorCategory(point.Metric),
				)
				l.Errorf("feed measurement: %s", err)
			}
		}

		select {
		case tt := <-tick.C:
			start = time.UnixMilli(inputs.AlignTimeMillSec(tt, start.UnixMilli(), ipt.Interval.Milliseconds()))

		case <-datakit.Exit.Wait():
			l.Infof("%s input exit", inputName)
			return
		case <-ipt.semStop.Wait():
			l.Infof("%s input return", inputName)
			return
		}
	}
}

func (ipt *Input) setup() {
	l = logger.SLogger(inputName)

	l.Infof("%s input started", inputName)
	ipt.Interval = config.ProtectedInterval(minInterval, maxInterval, ipt.Interval)
	ipt.mergedTags = inputs.MergeTags(ipt.tagger.HostTags(), ipt.Tags, "")
	l.Debugf("merged tags: %+#v", ipt.mergedTags)
}

func (ipt *Input) collect(ptTS int64) error {
	ipt.collectCache = make([]*point.Point, 0)
	opts := point.DefaultMetricOptions()
	opts = append(opts, point.WithTimestamp(ptTS))

	disks, partitions, err := ipt.diskStats.FilterUsage()
	if err != nil {
		return fmt.Errorf("error getting disk usage info: %w", err)
	}

	for index, du := range disks {
		if du == nil {
			l.Infof("no usage available, skip partition %+#v", partitions[index])
			continue
		}

		var kvs point.KVs

		if du.Total == 0 {
			// Skip dummy filesystem (procfs, cgroupfs, ...)
			continue
		}

		kvs = kvs.Add("device", partitions[index].Device, true, true)
		kvs = kvs.Add("fstype", du.Fstype, true, true)

		var usedPercent float64
		if du.Used+du.Free > 0 {
			usedPercent = float64(du.Used) /
				(float64(du.Used) + float64(du.Free)) * 100
		}
		kvs = kvs.Add("total", du.Total, false, true)
		kvs = kvs.Add("free", du.Free, false, true)
		kvs = kvs.Add("used", du.Used, false, true)
		kvs = kvs.Add("used_percent", usedPercent, false, true)

		switch runtime.GOOS {
		case datakit.OSLinux, datakit.OSDarwin:
			kvs = kvs.Add("inodes_total_mb", du.InodesTotal/1_000_000, false, true)
			kvs = kvs.Add("inodes_free_mb", du.InodesFree/1_000_000, false, true)
			kvs = kvs.Add("inodes_used_mb", du.InodesUsed/1_000_000, false, true)
			kvs = kvs.Add("inodes_used_percent", du.InodesUsedPercent, false, true) // float64
			kvs = kvs.Add("inodes_total", du.InodesTotal, false, true)              // Deprecated
			kvs = kvs.Add("inodes_free", du.InodesFree, false, true)                // Deprecated
			kvs = kvs.Add("inodes_used", du.InodesUsed, false, true)                // Deprecated
			kvs = kvs.Add("mount_point", partitions[index].Mountpoint, true, true)

			physicalDevices, err := ipt.findDisk(partitions[index].Device)
			if err == nil {
				kvs = kvs.Add("disk_name", strings.Join(physicalDevices, " "), true, true)

				if ipt.EnableLVMMapperPath && strings.HasPrefix(partitions[index].Device, "/dev/dm-") {
					mapperPath, err := GetMapperPath(partitions[index].Device)
					if err == nil {
						kvs.Add("device", mapperPath, true, true)
					} else {
						l.Error(err)
					}
				}
			} else {
				l.Error(err)
			}
		}

		for k, v := range ipt.mergedTags {
			kvs = kvs.AddTag(k, v)
		}

		ipt.collectCache = append(ipt.collectCache, point.NewPointV2(inputName, kvs, opts...))
	}

	return nil
}

func (ipt *Input) findDisk(partition string) ([]string, error) {
	if !strings.HasPrefix(partition, "/dev/") {
		return nil, fmt.Errorf("invalid partition path: %s", partition)
	}
	partitionName := strings.TrimPrefix(partition, "/dev/")

	if ipt.diskCache == nil {
		return nil, fmt.Errorf("diskCache cannot be nil")
	}

	// get disks from cache.
	if cachedEntry, ok := ipt.diskCache[partitionName]; ok {
		if time.Since(cachedEntry.LastUpdated) < 60*time.Second {
			return cachedEntry.Disks, nil
		}
	}

	var (
		disks []string
		err   error
	)

	if strings.HasPrefix(partitionName, "dm-") {
		disks, err = findDiskFromDM(partitionName)
	} else {
		disks, err = findDiskFromBlock(partitionName)
	}
	if err != nil {
		return nil, err
	}
	// update cache.
	ipt.diskCache[partitionName] = DiskCacheEntry{Disks: disks, LastUpdated: time.Now()}

	return disks, nil
}

func (*Input) Singleton() {}

func (ipt *Input) Terminate() {
	if ipt.semStop != nil {
		ipt.semStop.Close()
	}
}
func (*Input) Catalog() string          { return "host" }
func (*Input) SampleConfig() string     { return sampleCfg }
func (*Input) AvailableArchs() []string { return datakit.AllOS }
func (*Input) SampleMeasurement() []inputs.Measurement {
	return []inputs.Measurement{
		&docMeasurement{},
	}
}

// Tags          map[string]string `toml:"tags"`
// ExtraDevice   []string          `toml:"extra_device"`
// ExcludeDevice []string          `toml:"exclude_device"`

// IgnoreZeroBytesDisk bool `toml:"ignore_zero_bytes_disk"`
// OnlyPhysicalDevice  bool `toml:"only_physical_device"`

func (ipt *Input) GetENVDoc() []*inputs.ENVInfo {
	// nolint:lll
	infos := []*inputs.ENVInfo{
		{FieldName: "Interval"},
		{FieldName: "ExtraDevice", Type: doc.List, Example: "`/nfsdata,other_data`", Desc: "Additional device prefix. (By default, collect all devices with dev as the prefix)", DescZh: "额外的设备前缀。（默认收集以 dev 为前缀的所有设备）"},
		{FieldName: "ExcludeDevice", Type: doc.List, Example: `/dev/loop0,/dev/loop1`, Desc: "Excluded device prefix. (By default, collect all devices with dev as the prefix)", DescZh: "排除的设备前缀。（默认收集以 dev 为前缀的所有设备）"},
		{FieldName: "OnlyPhysicalDevice", Type: doc.Boolean, Default: `false`, Desc: "Physical devices only (e.g. hard disks, cd-rom drives, USB keys), and ignore all others (e.g. memory partitions such as /dev/shm)", DescZh: "忽略非物理磁盘（如网盘、NFS 等，只采集本机硬盘/CD ROM/USB 磁盘等）"},
		{
			FieldName: "EnableLvmMapperPath", // do _not_ use EnableLVMMapperPath, the LVM will be splited to L_V_M.
			Type:      doc.Boolean,
			Default:   `false`,
			Desc:      "View the soft link corresponding to the device mapper (e.g. `/dev/dm-0` -> `/dev/mapper/vg/lv`)",
			DescZh:    "查看设备映射器对应的软链接（如 `/dev/dm-0` -> `/dev/mapper/vg/lv`）",
		},
		{FieldName: "MergeOnDevice", Type: doc.Boolean, Default: `true`, Desc: "merge disks that have the same device", DescZh: "合并有相同 device 的磁盘"},
		{FieldName: "Tags"},
	}

	return doc.SetENVDoc("ENV_INPUT_DISK_", infos)
}

// ReadEnv support envs：
//
//	ENV_INPUT_DISK_EXCLUDE_DEVICE : []string
//	ENV_INPUT_DISK_EXTRA_DEVICE : []string
//	ENV_INPUT_DISK_TAGS : "a=b,c=d"
//	ENV_INPUT_DISK_ONLY_PHYSICAL_DEVICE : bool
//	ENV_INPUT_DISK_INTERVAL : time.Duration
func (ipt *Input) ReadEnv(envs map[string]string) {
	if fsList, ok := envs["ENV_INPUT_DISK_EXTRA_DEVICE"]; ok {
		list := strings.Split(fsList, ",")
		l.Debugf("add extra_device from ENV: %v", fsList)
		ipt.ExtraDevice = append(ipt.ExtraDevice, list...)
	}
	if fsList, ok := envs["ENV_INPUT_DISK_EXCLUDE_DEVICE"]; ok {
		list := strings.Split(fsList, ",")
		l.Debugf("add exlude_device from ENV: %v", fsList)
		ipt.ExcludeDevice = append(ipt.ExcludeDevice, list...)
	}

	if tagsStr, ok := envs["ENV_INPUT_DISK_TAGS"]; ok {
		tags := config.ParseGlobalTags(tagsStr)
		for k, v := range tags {
			ipt.Tags[k] = v
		}
	}

	if str := envs["ENV_INPUT_DISK_ONLY_PHYSICAL_DEVICE"]; str != "" {
		ipt.OnlyPhysicalDevice = true
	}

	//   ENV_INPUT_DISK_INTERVAL : time.Duration
	//   ENV_INPUT_DISK_MOUNT_POINTS : []string
	if str, ok := envs["ENV_INPUT_DISK_INTERVAL"]; ok {
		da, err := time.ParseDuration(str)
		if err != nil {
			l.Warnf("parse ENV_INPUT_DISK_INTERVAL to time.Duration: %s, ignore", err)
		} else {
			ipt.Interval = config.ProtectedInterval(minInterval,
				maxInterval,
				da)
		}
	}

	if str := envs["ENV_INPUT_DISK_ENABLE_LVM_MAPPER_PATH"]; str != "" {
		ipt.EnableLVMMapperPath = true
	}

	if str := envs["ENV_INPUT_DISK_MERGE_ON_DEVICE"]; str != "" {
		if ok, _ := strconv.ParseBool(str); ok {
			ipt.MergeOnDevice = ok
		}
	}

	// Default setting: we have add the env HOST_ROOT in datakit.yaml by default
	// but some old deployments may not hava this ENV set.
	ipt.hostRoot = "/rootfs"

	// Deprecated: use ENV_HOST_ROOT
	if v := os.Getenv("HOST_ROOT"); v != "" {
		ipt.hostRoot = v
	}

	if v := os.Getenv("ENV_HOST_ROOT"); v != "" {
		ipt.hostRoot = v
	}
}

func defaultInput() *Input {
	ipt := &Input{
		Interval: time.Second * 10,

		// Default merge on same device that will not cost too many time series for common disk metrics
		MergeOnDevice: true,

		semStop: cliutils.NewSem(),
		Tags:    make(map[string]string),
		feeder:  dkio.DefaultFeeder(),
		tagger:  datakit.DefaultGlobalTagger(),
	}

	x := &PSDisk{ipt: ipt}
	ipt.diskStats = x

	ipt.diskCache = make(map[string]DiskCacheEntry)
	return ipt
}

func noinit() { //nolint:gochecknoinits
	inputs.Add(inputName, func() inputs.Input {
		return defaultInput()
	})
}
