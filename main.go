//go:build arm64

package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
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
	VFIO_GROUP_GET_DEVICE_FD = 0x3B6A

	VFIO_DEVICE_BIND_IOMMUFD = 0x3B76

	IOMMU_GET_HW_INFO              = 0x3b8a
	IOMMU_HW_INFO_FLAG_INPUT_TYPE  = 1 << 0
	IOMMU_HW_INFO_TYPE_DEFAULT     = 0
	IOMMU_HW_INFO_TYPE_ARM_SMMUV3  = 2
	IOMMU_HW_CAP_PCI_ATS_NOT_SUPPORTED = 1 << 3
)


type vfioDeviceBindIommufd struct {
	Argsz        uint32
	Flags        uint32
	Iommufd      int32
	OutDevid     uint32
	TokenUuidPtr uint64
}

type iommuHwInfo struct {
	Size            uint32
	Flags           uint32
	DevID           uint32
	DataLen         uint32
	DataUptr        uint64
	InOutDataType   uint32
	OutMaxPasidLog2 uint8
	Reserved        [3]uint8
	OutCapabilities uint64
}

type iommuHwInfoArmSmmuv3 struct {
	Flags    uint32
	Reserved uint32
	IDR      [6]uint32
	IIDR     uint32
	AIDR     uint32
}

type IOMMUCapabilities struct {
	SSIDSize       int
	PASIDSupported bool
	ATSSupported   bool
	OASBits        int
}

// -------------------------------------------------------------------------
// IOMMUFD HELPERS
// -------------------------------------------------------------------------

func getIOMMUCapabilities(iommuFd int, devID uint32) (IOMMUCapabilities, error) {
	var info iommuHwInfo
	info.Size = uint32(unsafe.Sizeof(info))
	info.DevID = devID
	info.Flags = IOMMU_HW_INFO_FLAG_INPUT_TYPE
	info.InOutDataType = IOMMU_HW_INFO_TYPE_ARM_SMMUV3

	var armInfo iommuHwInfoArmSmmuv3
	info.DataLen = uint32(unsafe.Sizeof(armInfo))
	info.DataUptr = uint64(uintptr(unsafe.Pointer(&armInfo)))

	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(iommuFd),
		uintptr(IOMMU_GET_HW_INFO),
		uintptr(unsafe.Pointer(&info)),
	)
	if errno != 0 {
		// generic fallback
		info.Flags = 0
		info.InOutDataType = IOMMU_HW_INFO_TYPE_DEFAULT
		info.DataLen = 0
		_, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(iommuFd), uintptr(IOMMU_GET_HW_INFO), uintptr(unsafe.Pointer(&info)))
		if errno != 0 {
			return IOMMUCapabilities{OASBits: 48}, fmt.Errorf("IOMMU_GET_HW_INFO failed: %v", errno)
		}
	}

	caps := IOMMUCapabilities{
		SSIDSize:       int(info.OutMaxPasidLog2),
		PASIDSupported: info.OutMaxPasidLog2 > 0,
		ATSSupported:   (info.OutCapabilities&IOMMU_HW_CAP_PCI_ATS_NOT_SUPPORTED) == 0,
		OASBits:        48,
	}

	// === FIXED OAS FROM IDR5 ===
	if info.InOutDataType == IOMMU_HW_INFO_TYPE_ARM_SMMUV3 {
		oasField := armInfo.IDR[5] & 0xF
		if oasField <= 5 {
			caps.OASBits = 32 + int(oasField*4)
			fmt.Printf("  [Debug] Raw IDR5.OAS field = %d → OAS: %d bits\n", oasField, caps.OASBits)
		} else {
			fmt.Printf("  [Warning] Invalid OAS field %d (using default 48 bits)\n", oasField)
		}
	}
	return caps, nil
}

func bindDevice(cdevPath string) (IOMMUCapabilities, error) {
	iommuFd, err := unix.Open("/dev/iommu", unix.O_RDWR, 0)
	if err != nil {
		return IOMMUCapabilities{}, fmt.Errorf("SMMUv3 requires --device /dev/iommu (iommufd): %v", err)
	}
	defer unix.Close(iommuFd)

	devFd, err := unix.Open(cdevPath, unix.O_RDWR, 0)
	if err != nil {
		return IOMMUCapabilities{}, fmt.Errorf("open device failed: %v", err)
	}
	defer unix.Close(devFd)


	var args24 vfioDeviceBindIommufd
	args24.Argsz = uint32(unsafe.Sizeof(args24))
	args24.Iommufd = int32(iommuFd)
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(devFd), uintptr(VFIO_DEVICE_BIND_IOMMUFD), uintptr(unsafe.Pointer(&args24)))
	if e == 0 {
		return getIOMMUCapabilities(iommuFd, args24.OutDevid)
	}

	return IOMMUCapabilities{}, fmt.Errorf("iommufd bind failed")
}

func getCdevPath(bdf string) (string, error) {
	vfioDevDir := fmt.Sprintf("/sys/bus/pci/devices/%s/vfio-dev", bdf)
	entries, err := os.ReadDir(vfioDevDir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "vfio") {
			return filepath.Join("/dev/vfio/devices", entry.Name()), nil
		}
	}
	return "", fmt.Errorf("no vfio dev")
}

func isSMMUv3Enabled() (bool, error) {
	dir := "/sys/class/iommu"
	files, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, f := range files {
		link := filepath.Join(dir, f.Name())
		target, _ := os.Readlink(link)
		if strings.Contains(target, "arm-smmu-v3") {
			return true, nil
		}
	}
	return false, nil
}

// -------------------------------------------------------------------------
// NUMA + PCI HOLE HELPERS
// -------------------------------------------------------------------------

func CalculatePCIHole64(bdfs []string, marginKiB uint64) (uint64, error) {
	hole := uint64(0)
	for _, bdf := range bdfs {
		memPath := fmt.Sprintf("/sys/bus/pci/devices/%s/resource", bdf)
		f, err := os.Open(memPath)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			parts := strings.Fields(scanner.Text())
			if len(parts) < 3 {
				continue
			}
			start, _ := strconv.ParseUint(parts[0], 0, 64)
			end, _ := strconv.ParseUint(parts[1], 0, 64)
			if end > (1 << 32) {
				hole += (end - start + 1) / 1024
			}
		}
		f.Close()
	}
	return hole + marginKiB, nil
}

func InferExtraNUMANodes(bdf string) ([]int, int, error) {
	mainNode, _ := getMainNode(bdf)
	extra := getMemoryLessNoCPUNodes()
	return extra, mainNode, nil
}

func getMainNode(bdf string) (int, error) {
	nodeLink := fmt.Sprintf("/sys/bus/pci/devices/%s/numa_node", bdf)
	data, err := os.ReadFile(nodeLink)
	if err != nil {
		return 0, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n, nil
}

func getMemoryLessNoCPUNodes() []int {
	nodes := getOnlineNodes()
	var extra []int
	for _, n := range nodes {
		if isMemoryLessNoCPU(n) {
			extra = append(extra, n)
		}
	}
	return extra
}

func getOnlineNodes() []int {
	var nodes []int
	for i := 0; i < 64; i++ {
		if _, err := os.Stat(fmt.Sprintf("/sys/devices/system/node/node%d", i)); err == nil {
			nodes = append(nodes, i)
		}
	}
	return nodes
}

func isMemoryLessNoCPU(node int) bool {
	return !nodeHasCPUs(node) && !nodeHasMemory(node)
}

func nodeHasCPUs(node int) bool {
	_, err := os.Stat(fmt.Sprintf("/sys/devices/system/node/node%d/cpu0", node))
	return err == nil
}

func nodeHasMemory(node int) bool {
	data, _ := os.ReadFile(fmt.Sprintf("/sys/devices/system/node/node%d/memory0", node))
	return len(data) > 0
}

func getNodesetString(mainNode int, extraNodes []int) string {
	all := append([]int{mainNode}, extraNodes...)
	sort.Ints(all)
	if len(all) == 1 {
		return strconv.Itoa(all[0])
	}
	return fmt.Sprintf("%d-%d", all[0], all[len(all)-1])
}

// -------------------------------------------------------------------------
// MAIN
// -------------------------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: gh_check <BDF1> [BDF2 ...]")
		os.Exit(1)
	}
	bdfs := os.Args[1:]

	if runtime.GOARCH != "arm64" {
		fmt.Println("Not on ARM64")
		return
	}

	smmuEnabled, _ := isSMMUv3Enabled()
	fmt.Printf("SMMUv3 enabled: %v\n", smmuEnabled)
	if !smmuEnabled {
		return
	}

	globalOAS := 48
	globalSSIDSize := 0

	for _, bdf := range bdfs {
		fmt.Printf("\n--- %s ---\n", bdf)
		cdevPath, _ := getCdevPath(bdf)
		caps, err := bindDevice(cdevPath)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			os.Exit(1)
		}

		if caps.OASBits > globalOAS {
			globalOAS = caps.OASBits
		}
		if caps.SSIDSize > globalSSIDSize {
			globalSSIDSize = caps.SSIDSize
		}

		fmt.Printf("  ATS supported: %v\n", caps.ATSSupported)
		fmt.Printf("  PASID supported: %v | SSID size: %d bits\n", caps.PASIDSupported, caps.SSIDSize)
		fmt.Printf("  OAS: %d bits\n", caps.OASBits)
	}

	marginKiB := uint64(1024 * 1024)
	holeSize, _ := CalculatePCIHole64(bdfs, marginKiB)
	fmt.Printf("\nComputed pcihole64: %d KiB\n", holeSize)

	if len(bdfs) > 0 && smmuEnabled {
		firstBDF := bdfs[0]
		busHex := strings.Split(firstBDF, ":")[1]
		busDec, _ := strconv.ParseInt(busHex, 16, 64)

		xml := fmt.Sprintf(`<iommu model='smmuv3'>
  <driver pciBus='%d' accel='on' ats='on' ril='off' pasid='on' oas='%d' ssidsize='%d'/>
</iommu>`, busDec, globalOAS, globalSSIDSize)
		fmt.Printf("\nSuggested libvirt IOMMU XML:\n%s\n", xml)

		extraNodes, mainNode, _ := InferExtraNUMANodes(firstBDF)
		if len(extraNodes) > 0 {
			var cells strings.Builder
			cells.WriteString("<numa>\n")
			for _, id := range extraNodes {
				cells.WriteString(fmt.Sprintf("  <cell id='%d' memory='0' unit='KiB'/>\n", id))
			}
			cells.WriteString("</numa>\n")
			nodeset := getNodesetString(mainNode, extraNodes)
			acpi := fmt.Sprintf("<acpi nodeset='%s'/>", nodeset)
			fmt.Printf("Suggested extra vNUMA XML:\n%s", cells.String())
			fmt.Printf("Suggested ACPI nodeset: %s\n", acpi)
		}
	}
}
