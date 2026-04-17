// Package inventory reads hardware information from sysfs and procfs.
// No dmidecode or external tools are required.
package inventory

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nemvince/fos-next/internal/netup"
)

// Info holds the hardware inventory collected from procfs/sysfs.
type Info struct {
	CPUModel  string
	CPUCores  int
	RAMBytes  int64
	DiskBytes int64
	MACs      []string
	UUID      string
}

// Collect reads hardware inventory from /proc and /sys.
func Collect() (*Info, error) {
	info := &Info{}

	cpu, err := readCPU()
	if err != nil {
		return nil, fmt.Errorf("cpu: %w", err)
	}
	info.CPUModel = cpu.model
	info.CPUCores = cpu.cores

	ram, err := readRAM()
	if err != nil {
		return nil, fmt.Errorf("ram: %w", err)
	}
	info.RAMBytes = ram

	disk, err := readDisk()
	if err != nil {
		// non-fatal — disk might not be accessible yet
		disk = 0
	}
	info.DiskBytes = disk

	nics, err := netup.ListNICs()
	if err == nil {
		for _, n := range nics {
			info.MACs = append(info.MACs, n.MAC)
		}
	}

	info.UUID = readUUID()

	return info, nil
}

// ------------------------------------------------------------------
// CPU
// ------------------------------------------------------------------

type cpuInfo struct {
	model  string
	cores  int
}

func readCPU() (cpuInfo, error) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return cpuInfo{}, err
	}
	defer f.Close()

	var info cpuInfo
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		k, v, _ := strings.Cut(line, ":")
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "model name":
			if info.model == "" {
				info.model = v
			}
		case "processor":
			info.cores++
		}
	}
	return info, scanner.Err()
}

// ------------------------------------------------------------------
// RAM
// ------------------------------------------------------------------

func readRAM() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kib * 1024, nil
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

// ------------------------------------------------------------------
// Disk
// ------------------------------------------------------------------

func readDisk() (int64, error) {
	const target = "/dev/sda"
	data, err := os.ReadFile("/sys/block/sda/size")
	if err != nil {
		// Try nvme0n1 as fallback
		data, err = os.ReadFile("/sys/block/nvme0n1/size")
		if err != nil {
			return 0, fmt.Errorf("could not read disk size: disk not found at %s or nvme0n1", target)
		}
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, err
	}
	return sectors * 512, nil
}

// ------------------------------------------------------------------
// UUID (machine-id or DMI product UUID via sysfs)
// ------------------------------------------------------------------

func readUUID() string {
	paths := []string{
		"/sys/class/dmi/id/product_uuid",
		"/etc/machine-id",
	}
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Clean(p))
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}
