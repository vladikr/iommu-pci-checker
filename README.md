# iommu-pci-checker

A lightweight, vendor-agnostic tool for ARM64 systems that inspects PCI devices for advanced IOMMU capabilities (SMMUv3, ATS, PASID) and generates helpful libvirt domain XML snippets for PCI passthrough in virtualized environments (QEMU/KVM/libvirt/KubeVirt).

The tool prefers the modern IOMMUFD interface when available and falls back to legacy VFIO group ioctl if needed.

## Features

- Detects ARM SMMUv3 presence
- Infers Output Address Size (OAS) from CPU ID registers
- Reads device config space via native IOMMUFD or legacy VFIO
- Reports ATS support/enablement and PASID capability (including SSID size)
- Calculates required `pcihole64` reservation based on device BARs
- Infers extra memory-less NUMA nodes for vNUMA exposure
- Outputs suggested `<iommu>` and `<numa>`/`<acpi>` XML fragments

## Building the container image

The tool is distributed as a minimal Alpine-based container image.

```bash
podman build -t iommu-pci-checker:latest -f Containerfile .
```

## Running the tool

The container needs read-only access to /sys and access to VFIO/IOMMU devices.
Adjust the --device flags according to your system (IOMMU groups and /dev/vfio/devices/* entries).

```bash
podman run --rm -it \
  --user $$   (id -u):   $$(id -g) \
  --security-opt seccomp=unconfined \
  --ulimit memlock=-1:-1 \
  --device /dev/vfio/vfio \
  --device /dev/vfio/<GROUP> \          # e.g. --device /dev/vfio/20 for group 20
  --device /dev/iommu \
  --device /dev/vfio/devices/<DEV> \    # e.g. --device /dev/vfio/devices/vfio0
  -v /sys:/sys:ro \
  iommu-pci-checker:latest <BDF1> [<BDF2> ...]
```
Example BDF format: 0009:01:00.0
Note: If native IOMMUFD is not available or the device is not yet bound, the tool will automatically fall back to the legacy VFIO group method (requires the appropriate group device node).

## Why this tool exists

Modern ARM64 platforms with SMMUv3 often require specific IOMMU driver options
(ATS, PASID, correct OAS, SSID size) and sometimes additional memory-less NUMA
cells to fully expose device capabilities to guests.

In many virtualization deployments (e.g., KubeVirt), the component that
configures the libvirt domain (virt-launcher) runs unprivileged and rootless.
Discovering these parameters therefore needs to happen entirely from userspace,
without requiring root privileges.

This tool collects portable userspace approaches—reading sysfs, using
VFIO/IOMMUFD ioctls, and inferring values from CPU registers—to automate the
discovery when the process is granted access to the necessary device nodes.

