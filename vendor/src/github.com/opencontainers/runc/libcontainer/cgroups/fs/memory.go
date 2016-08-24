// +build linux

package fs

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/configs"
)

type MemoryGroup struct {
	supportV2 bool
}

func (s *MemoryGroup) Name() string {
	return "memory"
}

func (s *MemoryGroup) Apply(d *cgroupData) (err error) {
	path, err := d.path("memory")
	if cgroups.IsV2Error(err) {
		s.supportV2 = true
	} else if err != nil && !cgroups.IsNotFound(err) {
		return
	}
	if memoryAssigned(d.config) {
		if path != "" {
			if subErr := os.MkdirAll(path, 0755); subErr != nil {
				err = subErr
				return
			}
			if cgroups.IsV2Error(err) {
				if subErr := d.addControllerForV2("memory", path); subErr != nil {
					err = subErr
					return
				}
			}
		}

		if err := s.Set(path, d.config); err != nil {
			return err
		}
	}

	defer func() {
		if err != nil && !cgroups.IsV2Error(err) {
			os.RemoveAll(path)
		}
	}()

	// We need to join memory cgroup after set memory limits, because
	// kmem.limit_in_bytes can only be set when the cgroup is empty.
	_, err = d.join("memory")
	if err != nil && !cgroups.IsNotFound(err) && !cgroups.IsV2Error(err) {
		return err
	}

	return nil
}

func (s *MemoryGroup) Set(path string, cgroup *configs.Cgroup) error {
	if cgroup.Resources.Memory != 0 {
		name := "memory.limit_in_bytes"
		if s.supportV2 {
			name = "memory.max"
		}
		if err := writeFile(path, name, strconv.FormatInt(cgroup.Resources.Memory, 10)); err != nil {
			return err
		}
	}

	if cgroup.Resources.MemoryReservation != 0 {
		name := "soft_limit_in_bytes"
		if s.supportV2 {
			name = "memory.low"
		}
		if err := writeFile(path, name, strconv.FormatInt(cgroup.Resources.MemoryReservation, 10)); err != nil {
			return err
		}
	}

	if cgroup.Resources.MemorySwap > 0 {
		name := "memory.memsw.limit_in_bytes"
		if s.supportV2 {
			name = "memory.swap.max"
		}
		if err := writeFile(path, name, strconv.FormatInt(cgroup.Resources.MemorySwap, 10)); err != nil {
			return err
		}
	}

	if s.supportV2 {
		return nil
	}

	if cgroup.Resources.KernelMemory > 0 {
		if err := writeFile(path, "memory.kmem.limit_in_bytes", strconv.FormatInt(cgroup.Resources.KernelMemory, 10)); err != nil {
			return err
		}
	}

	if cgroup.Resources.OomKillDisable {
		if err := writeFile(path, "memory.oom_control", "1"); err != nil {
			return err
		}
	}

	if cgroup.Resources.MemorySwappiness >= 0 && cgroup.Resources.MemorySwappiness <= 100 {
		if err := writeFile(path, "memory.swappiness", strconv.FormatInt(cgroup.Resources.MemorySwappiness, 10)); err != nil {
			return err
		}
	} else if cgroup.Resources.MemorySwappiness == -1 {
		return nil
	} else {
		return fmt.Errorf("invalid value:%d. valid memory swappiness range is 0100", cgroup.Resources.MemorySwappiness)
	}

	return nil
}

func (s *MemoryGroup) Remove(d *cgroupData) error {
	path, err := d.path("memory")
	if cgroups.IsV2Error(err) {
		err = nil
	}
	return removePath(path, err)
}

func (s *MemoryGroup) GetStats(path string, stats *cgroups.Stats) error {
	// Set stats from memory.stat.
	statsFile, err := os.Open(filepath.Join(path, "memory.stat"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer statsFile.Close()

	fileCacheName := "cache"
	swapName := "memsw"
	if s.supportV2 {
		fileCacheName = "file"
		swapName = "swap"
	}

	sc := bufio.NewScanner(statsFile)
	for sc.Scan() {
		t, v, err := getCgroupParamKeyValue(sc.Text())
		if err != nil {
			return fmt.Errorf("failed to parse memory.stat (%q)  %v", sc.Text(), err)
		}
		stats.MemoryStats.Stats[t] = v
	}
	stats.MemoryStats.Cache = stats.MemoryStats.Stats[fileCacheName]

	memoryUsage, err := s.getMemoryData(path, "")
	if err != nil {
		return err
	}
	stats.MemoryStats.Usage = memoryUsage
	swapUsage, err := s.getMemoryData(path, swapName)
	if err != nil {
		return err
	}
	stats.MemoryStats.SwapUsage = swapUsage

	if s.supportV2 {
		return nil
	}

	kernelUsage, err := s.getMemoryData(path, "kmem")
	if err != nil {
		return err
	}
	stats.MemoryStats.KernelUsage = kernelUsage

	return nil
}

func memoryAssigned(cgroup *configs.Cgroup) bool {
	return cgroup.Resources.Memory != 0 ||
		cgroup.Resources.MemoryReservation != 0 ||
		cgroup.Resources.MemorySwap > 0 ||
		cgroup.Resources.KernelMemory > 0 ||
		cgroup.Resources.OomKillDisable ||
		cgroup.Resources.MemorySwappiness != 1
}

func (s *MemoryGroup) getMemoryData(path, name string) (cgroups.MemoryData, error) {
	memoryData := cgroups.MemoryData{}

	moduleName := "memory"
	if name != "" {
		moduleName = strings.Join([]string{"memory", name}, ".")
	}

	usageName := "usage_in_bytes"
	if s.supportV2 {
		usageName = "current"
	}

	usage := strings.Join([]string{moduleName, usageName}, ".")
	value, err := getCgroupParamUint(path, usage)
	if err != nil {
		if moduleName != "memory" && os.IsNotExist(err) {
			return cgroups.MemoryData{}, nil
		}
		return cgroups.MemoryData{}, fmt.Errorf("failed to parse %s  %v", usage, err)
	}
	memoryData.Usage = value

	if s.supportV2 {
		return memoryData, nil
	}

	maxUsage := strings.Join([]string{moduleName, "max_usage_in_bytes"}, ".")
	failcnt := strings.Join([]string{moduleName, "failcnt"}, ".")
	value, err = getCgroupParamUint(path, maxUsage)
	if err != nil {
		if moduleName != "memory" && os.IsNotExist(err) {
			return cgroups.MemoryData{}, nil
		}
		return cgroups.MemoryData{}, fmt.Errorf("failed to parse %s  %v", maxUsage, err)
	}
	memoryData.MaxUsage = value
	value, err = getCgroupParamUint(path, failcnt)
	if err != nil {
		if moduleName != "memory" && os.IsNotExist(err) {
			return cgroups.MemoryData{}, nil
		}
		return cgroups.MemoryData{}, fmt.Errorf("failed to parse %s  %v", failcnt, err)
	}
	memoryData.Failcnt = value

	return memoryData, nil
}
