//go:build arm64

package main

/*
#include <stdint.h>
uint64_t read_id_aa64mmfr0() {
    uint64_t id;
    // #nosec G103
    __asm__ volatile ("mrs %0, ID_AA64MMFR0_EL1" : "=r" (id));
    return id;
}
*/
import "C"

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// -------------------------------------------------------------------------
// CONSTANTS & STRUCTS
// -------------------------------------------------------------------------

const (
	// Legacy VFIO Constants
	VFIO_GROUP_GET_DEVICE_FD = 0x3B6A
	VFIO_CHECK_EXTENSION     = 0x3B65
	VFIO_SET_IOMMU           = 0x3B66
	VFIO_GROUP_SET_CONTAINER = 0x3B68
	VFIO_TYPE1v2_IOMMU       = 3

	// Native IOMMUFD Constants (Mainline Linux - extensible)
	VFIO_DEVICE_BIND_IOMMUFD = 0x3B76  // _IO(VFIO_TYPE, VFIO_BASE + 18)
)


// Standard 16-byte struct
type vfioDeviceBindIommufd16 struct {
	Argsz    uint32
	Flags    uint32
	Iommufd  int32
	OutDevid uint32
}

// Extended 24-byte struct
type vfioDeviceBindIommufd24 struct {
	Argsz        uint32
	Flags        uint32
	Iommufd      int32
	OutDevid     uint32
	TokenUuidPtr uint64
}

type bindNoArgsz32 struct {
	Flags    uint32
	Iommufd  int32
	OutDevid uint32
	_        uint32 // padding if needed
}

type bindNoArgsz64 struct {
	Flags    uint32
	Iommufd  int32
	OutDevid uint64
}


// -------------------------------------------------------------------------
// MAIN EXECUTION
// -------------------------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run main.go <BDF1> [<BDF2> ...]")
		os.Exit(1)
	}
	bdfs := os.Args[1:]

	if runtime.GOARCH != "arm64" {
		fmt.Println("Not on ARM64 architecture, skipping SMMUv3 checks.")
		return
	}

	// 1. Check SMMUv3
	smmuEnabled, err := isSMMUv3Enabled()
	if err != nil {
		fmt.Printf("Error checking SMMUv3: %v\n", err)
		return
	}
	fmt.Printf("ARM SMMUv3 enabled: %v\n", smmuEnabled)

	if !smmuEnabled {
		fmt.Println("SMMUv3 not enabled, skipping further checks.")
		return
	}

	// 2. Check OAS
	oas, err := getOASFromCPU()
	if err != nil {
		fmt.Printf("Error getting OAS: %v\n", err)
		oas = 48
	}
	fmt.Printf("Inferred OAS: %d\n", oas)

	// 3. Process each BDF
	for _, bdf := range bdfs {
		fmt.Printf("\n--- Processing %s ---\n", bdf)
		atsSupported, atsEnabled, pasidSupported, ssidSize, err := parseConfigHybrid(bdf)
		if err != nil {
			fmt.Printf("Failed to process %s: %v\n", bdf, err)
			continue
		}
		fmt.Printf("  ATS supported: %v, enabled: %v\n", atsSupported, atsEnabled)
		fmt.Printf("  PASID supported: %v, ssidSize: %d\n", pasidSupported, ssidSize)
	}

	// 4. Calculate Hole
	marginKiB := uint64(1024 * 1024)
	holeSize, err := CalculatePCIHole64(bdfs, marginKiB)
	if err != nil {
		fmt.Printf("Error calculating pcihole64: %v\n", err)
		return
	}
	fmt.Printf("\nComputed pcihole64: %d KiB\n", holeSize)

	// 5. XML Generation (Example for first BDF)
	if len(bdfs) > 0 {
		firstBDF := bdfs[0]
		busHex := strings.Split(firstBDF, ":")[1]
		busDec, _ := strconv.ParseInt(busHex, 16, 64)
		atsSupported, _, pasidSupported, ssidSize, _ := parseConfigHybrid(firstBDF)
		extraNodes, mainNode, _ := InferExtraNUMANodes(firstBDF)

		if smmuEnabled && atsSupported && pasidSupported {
			xml := fmt.Sprintf(`<iommu model='smmuv3'>
  <driver pciBus='%d' accel='on' ats='on' ril='off' pasid='on' oas='%d' ssidsize='%d'/>
</iommu>`, busDec, oas, ssidSize)
			fmt.Printf("\nSuggested IOMMU XML: %s\n", xml)
		}

		if len(extraNodes) > 0 {
			var cells strings.Builder
			cells.WriteString("<numa>\n")
			for _, id := range extraNodes {
				cells.WriteString(fmt.Sprintf("  <cell id='%d' memory='0' unit='KiB'/>\n", id))
			}
			cells.WriteString("</numa>\n")
			nodeset := getNodesetString(mainNode, extraNodes)
			acpi := fmt.Sprintf("<acpi nodeset='%s'/>", nodeset)
			fmt.Printf("Suggested extra vNUMA XML: %s\n", cells.String())
			fmt.Printf("Suggested ACPI nodeset: %s\n", acpi)
		}
	}
}

// -------------------------------------------------------------------------
// HYBRID CONFIG READER
// -------------------------------------------------------------------------

func parseConfigHybrid(bdf string) (atsSupported, atsEnabled, pasidSupported bool, ssidSize int, err error) {
	// Strategy:
	// 1. Try Native IOMMUFD (Preferred)
	// 2. If Native fails/unavailable, Try Legacy VFIO (Fallback)
	
	var data []byte

	// Try Native
	cdevPath, _ := getCdevPath(bdf)
	if cdevPath != "" {
		fmt.Printf("  Attempting Native IOMMUFD (%s)...\n", cdevPath)
		data, err = readConfigNative(cdevPath)
		if err == nil {
			fmt.Println("  [Native] Success.")
		} else {
			fmt.Printf("  [Native] Failed: %v. Switching to Legacy.\n", err)
		}
	}

	// Try Legacy if Native failed
	if data == nil {
		fmt.Println("  Attempting Legacy VFIO Group...")
		data, err = readConfigLegacy(bdf)
		if err != nil {
			return false, false, false, 0, fmt.Errorf("both Native and Legacy methods failed: %v", err)
		}
		fmt.Println("  [Legacy] Success.")
	}

	// Parse Capabilities
	return parseCapabilities(data)
}

func parseCapabilities(data []byte) (ats, atsEn, pasid bool, ssid int, err error) {
	if len(data) < 0x1000 {
		return false, false, false, 0, fmt.Errorf("config space too small: %d bytes", len(data))
	}

	offset := uint32(0x100)
	for offset != 0 && int(offset) < len(data) {
		header := binary.LittleEndian.Uint32(data[offset : offset+4])
		capID := uint16(header & 0xffff)
		next := uint32((header >> 20) & 0xfff)

		if capID == 0x0f { // ATS
			ats = true
			ctrl := binary.LittleEndian.Uint16(data[offset+6 : offset+8])
			atsEn = (ctrl & 0x8000) != 0
		} else if capID == 0x1b { // PASID
			pasid = true
			capReg := binary.LittleEndian.Uint16(data[offset+4 : offset+6])
			ssid = int((capReg >> 8) & 0x1f)
		}

		if next < 0x100 { break }
		offset = next
	}
	if !pasid { fmt.Println("  [Info] PASID capability missing.") }
	return
}

// -------------------------------------------------------------------------
// NATIVE STRATEGY
// -------------------------------------------------------------------------

func readConfigNative(cdevPath string) ([]byte, error) {
	iommuFd, err := unix.Open("/dev/iommu", unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/iommu failed: %v", err)
	}
	defer unix.Close(iommuFd)

	devFd, err := unix.Open(cdevPath, unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open cdev failed: %v", err)
	}
	defer unix.Close(devFd)

	// ADAPTIVE BIND: Try 16-byte, then 24-byte
	if err := adaptiveBind(devFd, iommuFd); err != nil {
		return nil, err
	}

	return readConfigFromFD(devFd)
}

func adaptiveBind(devFd, iommuFd int) error {
	// Try mainline base (16-byte) first - this should succeed on recent kernels
	var args16 vfioDeviceBindIommufd16
	args16.Argsz = uint32(unsafe.Sizeof(args16))  // 16
	args16.Flags = 0
	args16.Iommufd = int32(iommuFd)
	args16.OutDevid = 0

	_, _, e1 := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(devFd),
		uintptr(VFIO_DEVICE_BIND_IOMMUFD),
		uintptr(unsafe.Pointer(&args16)),
	)
	if e1 == 0 {
		fmt.Printf("  [Bind] Success with mainline 16-byte (dev_id: %d).\n", args16.OutDevid)
		return nil
	}
	fmt.Printf("  [Bind] 16-byte failed (%v), trying 24-byte extension...\n", e1)

	// Fallback: if kernel requires the full struct space (rare for basic bind)
	if e1 == unix.EINVAL || e1 == unix.E2BIG || e1 == unix.ENOTTY {
		var args24 vfioDeviceBindIommufd24
		args24.Argsz = uint32(unsafe.Sizeof(args24))  // 24
		args24.Flags = 0
		args24.Iommufd = int32(iommuFd)
		args24.OutDevid = 0
		args24.TokenUuidPtr = 0  // No token needed

		_, _, e2 := unix.Syscall(
			unix.SYS_IOCTL,
			uintptr(devFd),
			uintptr(VFIO_DEVICE_BIND_IOMMUFD),
			uintptr(unsafe.Pointer(&args24)),
		)
		if e2 == 0 {
			fmt.Printf("  [Bind] Success with mainline 24-byte extension (dev_id: %d).\n", args24.OutDevid)
			return nil
		}
		return fmt.Errorf("both 16-byte (%v) and 24-byte (%v) failed", e1, e2)
	}

	return fmt.Errorf("16-byte bind failed (no fallback): %v", e1)
}

// -------------------------------------------------------------------------
// LEGACY STRATEGY
// -------------------------------------------------------------------------

func readConfigLegacy(bdf string) ([]byte, error) {
	groupLink := fmt.Sprintf("/sys/bus/pci/devices/%s/iommu_group", bdf)
	target, err := os.Readlink(groupLink)
	if err != nil {
		return nil, fmt.Errorf("readlink iommu_group failed: %v", err)
	}
	group := filepath.Base(target)

	containerFd, err := unix.Open("/dev/vfio/vfio", unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/vfio/vfio failed: %v", err)
	}
	defer unix.Close(containerFd)

	// Check Extension (Best effort)
	unix.Syscall(unix.SYS_IOCTL, uintptr(containerFd), VFIO_CHECK_EXTENSION, uintptr(VFIO_TYPE1v2_IOMMU))

	groupPath := filepath.Join("/dev/vfio", group)
	groupFd, err := unix.Open(groupPath, unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open group %s failed: %v", groupPath, err)
	}
	defer unix.Close(groupFd)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(groupFd), VFIO_GROUP_SET_CONTAINER, uintptr(unsafe.Pointer(&containerFd)))
	if errno != 0 {
		return nil, fmt.Errorf("SET_CONTAINER failed: %v", errno)
	}

	_, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(containerFd), VFIO_SET_IOMMU, uintptr(VFIO_TYPE1v2_IOMMU))
	if errno != 0 {
		return nil, fmt.Errorf("SET_IOMMU failed: %v", errno)
	}

	devFd, err := ioctlGetDeviceFd(groupFd, bdf)
	if err != nil {
		return nil, fmt.Errorf("GET_DEVICE_FD failed: %v", err)
	}
	defer unix.Close(devFd)

	return readConfigFromFD(devFd)
}

// -------------------------------------------------------------------------
// COMMON HELPERS
// -------------------------------------------------------------------------

func readConfigFromFD(devFd int) ([]byte, error) {
	data := make([]byte, 4096)
	n, err := unix.Pread(devFd, data, 7<<40) // Region 7
	if err != nil {
		return nil, fmt.Errorf("pread failed: %v", err)
	}
	return data[:n], nil
}

func getCdevPath(bdf string) (string, error) {
	vfioDevDir := fmt.Sprintf("/sys/bus/pci/devices/%s/vfio-dev", bdf)
	entries, err := os.ReadDir(vfioDevDir)
	if err != nil { return "", err }
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "vfio") {
			return filepath.Join("/dev/vfio/devices", entry.Name()), nil
		}
	}
	return "", fmt.Errorf("no vfio dev found")
}

func ioctlGetDeviceFd(groupFd int, deviceName string) (int, error) {
	cStr, err := unix.BytePtrFromString(deviceName)
	if err != nil { return 0, err }
	fd, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(groupFd), uintptr(VFIO_GROUP_GET_DEVICE_FD), uintptr(unsafe.Pointer(cStr)))
	if errno != 0 { return 0, errno }
	return int(fd), nil
}

func isSMMUv3Enabled() (bool, error) {
    dir := "/sys/class/iommu"
    files, err := os.ReadDir(dir)
    if err != nil { return false, err }
    for _, f := range files {
        link := filepath.Join(dir, f.Name())
        target, err := os.Readlink(link)
        if err == nil && strings.Contains(target, "arm-smmu-v3") { return true, nil }
    }
    return false, nil
}

func getOASFromCPU() (int, error) {
	id := readIDAA64MMFR0()
	parange := id & 0xf
	oasMap := map[uint64]int{ 0: 32, 1: 36, 2: 40, 3: 44, 4: 48, 5: 52, 6: 56, 7: 64 }
	if oas, ok := oasMap[parange]; ok {
	    if oas < 40 { return 0, fmt.Errorf("unlikely low PARange value: %d (oas=%d)", parange, oas) }
		return oas, nil
	}
	return 0, fmt.Errorf("unknown PARange value: %d", parange)
}

func readIDAA64MMFR0() uint64 { return uint64(C.read_id_aa64mmfr0()) }

func CalculatePCIHole64(bdfs []string, marginKiB uint64) (uint64, error) {
    var totalSize uint64
    for _, bdf := range bdfs {
        resourcePath := fmt.Sprintf("/sys/bus/pci/devices/%s/resource", bdf)
        file, err := os.Open(resourcePath)
        if err != nil { return 0, fmt.Errorf("open resource failed: %w", err) }
        defer file.Close()
        scanner := bufio.NewScanner(file)
        for scanner.Scan() {
            fields := strings.Fields(scanner.Text())
            if len(fields) != 3 { continue }
            fields[0] = strings.TrimPrefix(fields[0], "0x")
            fields[1] = strings.TrimPrefix(fields[1], "0x")
            fields[2] = strings.TrimPrefix(fields[2], "0x")
            start, _ := strconv.ParseUint(fields[0], 16, 64)
            end, _ := strconv.ParseUint(fields[1], 16, 64)
            flags, _ := strconv.ParseUint(fields[2], 16, 64)
            if start == 0 || end == 0 || start > end { continue }
            if (flags & 0x00000200) != 0x00000200 || (flags & 0xf) != 0xc { continue }
            totalSize += (end - start + 1)
        }
    }
    totalSizeKiB := totalSize / 1024
    totalSizeKiB += marginKiB
    if totalSizeKiB == 0 { return 0, nil }
    holeSize := uint64(1)
    for holeSize < totalSizeKiB { holeSize <<= 1 }
    return holeSize, nil
}

func InferExtraNUMANodes(bdf string) (extraNodes []int, mainNode int, err error) {
	numaPath := fmt.Sprintf("/sys/bus/pci/devices/%s/numa_node", bdf)
	numaData, err := os.ReadFile(numaPath)
	if err != nil { return nil, -1, err }
	mainNode, err = strconv.Atoi(strings.TrimSpace(string(numaData)))
	if err != nil { return nil, -1, err }
	if mainNode < 0 { return nil, mainNode, fmt.Errorf("invalid main NUMA node: %d", mainNode) }
	
	onlineNodes, _ := getOnlineNodes()
	allMemoryLess, _ := getMemoryLessNoCPUNodes(onlineNodes)
	hasCPUs, _ := nodeHasCPUs(mainNode)
	
	if hasCPUs {
		sort.Ints(allMemoryLess)
		return allMemoryLess, mainNode, nil
	} else {
		extraNodes = []int{}
		candidate := mainNode + 1
		maxExtra := 16
		for len(extraNodes) < maxExtra {
			if !contains(onlineNodes, candidate) { break }
			isMemoryLess, _ := isMemoryLessNoCPU(candidate)
			if !isMemoryLess { break }
			extraNodes = append(extraNodes, candidate)
			candidate++
		}
		return extraNodes, mainNode, nil
	}
}

func getOnlineNodes() ([]int, error) {
	data, err := os.ReadFile("/sys/devices/system/node/online")
	if err != nil { return nil, err }
	var nodes []int
	for _, part := range strings.Split(strings.TrimSpace(string(data)), ",") {
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			start, _ := strconv.Atoi(rangeParts[0])
			end, _ := strconv.Atoi(rangeParts[1])
			for i := start; i <= end; i++ { nodes = append(nodes, i) }
		} else {
			node, _ := strconv.Atoi(part)
			nodes = append(nodes, node)
		}
	}
	sort.Ints(nodes)
	return nodes, nil
}
func getMemoryLessNoCPUNodes(nodes []int) ([]int, error) {
	var memoryLess []int
	for _, node := range nodes { is, _ := isMemoryLessNoCPU(node); if is { memoryLess = append(memoryLess, node) } }
	return memoryLess, nil
}
func isMemoryLessNoCPU(node int) (bool, error) {
	memData, err := os.ReadFile(fmt.Sprintf("/sys/devices/system/node/node%d/meminfo", node))
	if err != nil { return false, err }
	memTotalRe := regexp.MustCompile(`MemTotal:\s+(\d+)\s+kB`)
	match := memTotalRe.FindStringSubmatch(string(memData))
	if len(match) != 2 || match[1] != "0" { return false, nil }
	cpuData, err := os.ReadFile(fmt.Sprintf("/sys/devices/system/node/node%d/cpulist", node))
	if err != nil { return false, err }
	cpuStr := strings.TrimSpace(string(cpuData))
	if cpuStr != "" && cpuStr != "none" { return false, nil }
	return true, nil
}
func nodeHasCPUs(node int) (bool, error) {
	cpuData, err := os.ReadFile(fmt.Sprintf("/sys/devices/system/node/node%d/cpulist", node))
	if err != nil { return false, err }
	cpuStr := strings.TrimSpace(string(cpuData))
	return cpuStr != "" && cpuStr != "none", nil
}
func contains(slice []int, val int) bool {
	for _, item := range slice { if item == val { return true } }
	return false
}
func getNodesetString(main int, extras []int) string {
	all := append(extras, main)
	sort.Ints(all)
	if len(all) == 0 { return "" }
	min, max := all[0], all[len(all)-1]
	if max-min+1 == len(all) { return fmt.Sprintf("%d-%d", min, max) }
	var parts []string
	for _, n := range all { parts = append(parts, strconv.Itoa(n)) }
	return strings.Join(parts, ",")
}
