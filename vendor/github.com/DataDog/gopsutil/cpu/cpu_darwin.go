// +build darwin

package cpu

import (
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// sys/resource.h
const (
	CPUser    = 0
	CPNice    = 1
	CPSys     = 2
	CPIntr    = 3
	CPIdle    = 4
	CPUStates = 5
)

// default value. from time.h
var ClocksPerSec = float64(128)

func Times(percpu bool) ([]TimesStat, error) {
	if percpu {
		return perCPUTimes()
	}

	return allCPUTimes()
}

// Returns only one CPUInfoStat on FreeBSD
func Info() ([]InfoStat, error) {
	var ret []InfoStat

	c := InfoStat{}
	c.ModelName, _ = unix.Sysctl("machdep.cpu.brand_string")
	family, _ := unix.SysctlUint32("machdep.cpu.family")
	c.Family = strconv.FormatUint(uint64(family), 10)
	model, _ := unix.SysctlUint32("machdep.cpu.model")
	c.Model = strconv.FormatUint(uint64(model), 10)
	stepping, _ := unix.SysctlUint32("machdep.cpu.stepping")
	c.Stepping = int32(stepping)
	features, err := unix.Sysctl("machdep.cpu.features")
	if err == nil {
		for _, v := range strings.Fields(features) {
			c.Flags = append(c.Flags, strings.ToLower(v))
		}
	}
	leaf7Features, err := unix.Sysctl("machdep.cpu.leaf7_features")
	if err == nil {
		for _, v := range strings.Fields(leaf7Features) {
			c.Flags = append(c.Flags, strings.ToLower(v))
		}
	}
	extfeatures, err := unix.Sysctl("machdep.cpu.extfeatures")
	if err == nil {
		for _, v := range strings.Fields(extfeatures) {
			c.Flags = append(c.Flags, strings.ToLower(v))
		}
	}
	cores, _ := unix.SysctlUint32("machdep.cpu.core_count")
	c.Cores = int32(cores)
	cacheSize, _ := unix.SysctlUint32("machdep.cpu.cache.size")
	c.CacheSize = int32(cacheSize)
	c.VendorID, _ = unix.Sysctl("machdep.cpu.vendor")

	// Use the rated frequency of the CPU. This is a static value and does not
	// account for low power or Turbo Boost modes.
	cpuFrequency, err := unix.SysctlUint64("hw.cpufrequency")
	if err == nil {
		c.Mhz = float64(cpuFrequency) / 1000000.0
	}

	return append(ret, c), nil
}
