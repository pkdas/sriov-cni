package utils

import (
	"fmt"
	"github.com/vishvananda/netlink"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SRIOVDevice : for supporting misc NIC types
type SRIOVDevice struct {
	name     string
	vendor   string
	device   string
	driver   string
	dpdkbind bool
}

var (
	// SRIOVDeviceList : list of SRIOV devices supported
	SRIOVDeviceList = []SRIOVDevice{
		SRIOVDevice{
			name:     "i40e",
			vendor:   "test",
			device:   "test",
			driver:   "test",
			dpdkbind: true,
		},
		SRIOVDevice{
			name:     "ConnectX-5",
			vendor:   "0x15b3",
			device:   "0x1017",
			driver:   "mlx5_core",
			dpdkbind: false,
		},
		SRIOVDevice{
			name:     "ConnectX-5-VF",
			vendor:   "0x15b3",
			device:   "0x1018",
			driver:   "mlx5_core",
			dpdkbind: false,
		},
	}
	sriovConfigured = "/sriov_numvfs"
	// NetDirectory sysfs net directory
	NetDirectory = "/sys/class/net"
	// SysBusPci is sysfs pci device directory
	SysBusPci = "/sys/bus/pci/devices"
	// UserspaceDrivers is a list of driver names that don't have netlink representation for their devices
	UserspaceDrivers = []string{"vfio-pci", "uio_pci_generic", "igb_uio"}
)

// GetSriovNumVfs takes in a PF name(ifName) as string and returns number of VF configured as int
func GetSriovNumVfs(ifName string) (int, error) {
	var vfTotal int

	sriovFile := filepath.Join(NetDirectory, ifName, "device", sriovConfigured)
	if _, err := os.Lstat(sriovFile); err != nil {
		return vfTotal, fmt.Errorf("failed to open the sriov_numfs of device %q: %v", ifName, err)
	}

	data, err := ioutil.ReadFile(sriovFile)
	if err != nil {
		return vfTotal, fmt.Errorf("failed to read the sriov_numfs of device %q: %v", ifName, err)
	}

	if len(data) == 0 {
		return vfTotal, fmt.Errorf("no data in the file %q", sriovFile)
	}

	sriovNumfs := strings.TrimSpace(string(data))
	vfTotal, err = strconv.Atoi(sriovNumfs)
	if err != nil {
		return vfTotal, fmt.Errorf("failed to convert sriov_numfs(byte value) to int of device %q: %v", ifName, err)
	}

	return vfTotal, nil
}

// GetVfid takes in VF's PCI address(addr) and pfName as string and returns VF's ID as int
func GetVfid(addr string, pfName string) (int, error) {
	var id int
	vfTotal, err := GetSriovNumVfs(pfName)
	if err != nil {
		return id, err
	}
	for vf := 0; vf <= vfTotal; vf++ {
		vfDir := filepath.Join(NetDirectory, pfName, "device", fmt.Sprintf("virtfn%d", vf))
		_, err := os.Lstat(vfDir)
		if err != nil {
			continue
		}
		pciinfo, err := os.Readlink(vfDir)
		if err != nil {
			continue
		}
		pciaddr := filepath.Base(pciinfo)
		if pciaddr == addr {
			return vf, nil
		}
	}
	return id, fmt.Errorf("unable to get VF ID with PF: %s and VF pci address %v", pfName, addr)
}

// GetPfName returns PF net device name of a given VF pci address
func GetPfName(vf string) (string, error) {
	pfSymLink := filepath.Join(SysBusPci, vf, "physfn", "net")
	_, err := os.Lstat(pfSymLink)
	if err != nil {
		return "", err
	}

	files, err := ioutil.ReadDir(pfSymLink)
	if err != nil {
		return "", err
	}

	if len(files) < 1 {
		return "", fmt.Errorf("PF network device not found")
	}

	return strings.TrimSpace(files[0].Name()), nil
}

// GetPciAddress takes in a interface(ifName) and VF id and returns returns its pci addr as string
func GetPciAddress(ifName string, vf int) (string, error) {
	var pciaddr string
	vfDir := filepath.Join(NetDirectory, ifName, "device", fmt.Sprintf("virtfn%d", vf))
	dirInfo, err := os.Lstat(vfDir)
	if err != nil {
		return pciaddr, fmt.Errorf("can't get the symbolic link of virtfn%d dir of the device %q: %v", vf, ifName, err)
	}

	if (dirInfo.Mode() & os.ModeSymlink) == 0 {
		return pciaddr, fmt.Errorf("No symbolic link for the virtfn%d dir of the device %q", vf, ifName)
	}

	pciinfo, err := os.Readlink(vfDir)
	if err != nil {
		return pciaddr, fmt.Errorf("can't read the symbolic link of virtfn%d dir of the device %q: %v", vf, ifName, err)
	}

	pciaddr = filepath.Base(pciinfo)
	return pciaddr, nil
}

// GetSharedPF takes in VF name(ifName) as string and returns the other VF name that shares same PCI address as string
func GetSharedPF(ifName string) (string, error) {
	pfName := ""
	pfDir := filepath.Join(NetDirectory, ifName)
	dirInfo, err := os.Lstat(pfDir)
	if err != nil {
		return pfName, fmt.Errorf("can't get the symbolic link of the device %q: %v", ifName, err)
	}

	if (dirInfo.Mode() & os.ModeSymlink) == 0 {
		return pfName, fmt.Errorf("No symbolic link for dir of the device %q", ifName)
	}

	fullpath, err := filepath.EvalSymlinks(pfDir)
	parentDir := fullpath[:len(fullpath)-len(ifName)]
	dirList, err := ioutil.ReadDir(parentDir)

	for _, file := range dirList {
		if file.Name() != ifName {
			pfName = file.Name()
			return pfName, nil
		}
	}

	return pfName, fmt.Errorf("Shared PF not found")
}

// ShouldHaveNetlink determines whether VF is expected to have a netlink interface
func ShouldHaveNetlink(pfName string, vfID int) (bool, error) {
	driverLink := filepath.Join(NetDirectory, pfName, "device", fmt.Sprintf("virtfn%d", vfID), "driver")
	driverPath, err := filepath.EvalSymlinks(driverLink)
	if err != nil {
		return false, err
	}
	driverStat, err := os.Stat(driverPath)
	if err != nil {
		return false, err
	}
	driverName := driverStat.Name()
	for _, drv := range UserspaceDrivers {
		if driverName == drv {
			return false, nil
		}
	}
	return true, nil
}

// GetVFLinkNames returns VF's network interface name given it's PF name as string and VF id as int
func GetVFLinkNames(pfName string, vfID int) ([]string, error) {
	var names []string
	vfDir := filepath.Join(NetDirectory, pfName, "device", fmt.Sprintf("virtfn%d", vfID), "net")
	if _, err := os.Lstat(vfDir); err != nil {
		return nil, err
	}

	fInfos, err := ioutil.ReadDir(vfDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read the virtfn%d dir of the device %q: %v", vfID, pfName, err)
	}

	names = make([]string, 0)
	for _, f := range fInfos {
		names = append(names, f.Name())
	}

	return names, nil
}

// GetDPDKbind returns for VF the net driver and if dpdkbind is required
func GetDPDKbind(addr string, pfName string, vfID int) (bool, string, error) {
	driverLink := filepath.Join(NetDirectory, pfName, "device", fmt.Sprintf("virtfn%d", vfID), "driver")
	driverPath, err := filepath.EvalSymlinks(driverLink)
	if err != nil {
		return false, "", err
	}
	driverStat, err := os.Stat(driverPath)
	if err != nil {
		return false, "", err
	}
	driverName := driverStat.Name()
	for _, sriovDevice := range SRIOVDeviceList {
		if driverName == sriovDevice.driver {
			return sriovDevice.dpdkbind, sriovDevice.driver, nil
		}
	}
	// for now return true and no error
	// after filling SRIOVDeviceList with correct i40e device/driver info
	// this should return false and error
	return true, "", nil
}

// GetVFHwAddress returns VF's mac address given it's PF name as string and VF id as int
func GetVFHwAddress(pfName string, vfID int) (string, error) {
	ifNames, _ := GetVFLinkNames(pfName, vfID)

	ifName := ifNames[0]
	addressFile := filepath.Join(NetDirectory, ifName, "address")

	if _, err := os.Lstat(addressFile); err != nil {
		return "", fmt.Errorf("failed to open the HW address of device %q: pfname %q vf %d %v", ifName, pfName, vfID, err)
	}

	data, err := ioutil.ReadFile(addressFile)
	if err != nil {
		return "", fmt.Errorf("failed to read the HW address of device %q:  pfname %q vf %d %v", ifName, pfName, vfID, err)
	}

	if len(data) == 0 {
		return "", fmt.Errorf("no data in the file %q", addressFile)
	}

	hwAddress := strings.TrimSpace(string(data))
	return hwAddress, nil
}

// SetVFHwAddress sets VF's mac address given it's PF name as string and VF id as int
func SetVFHwAddress(pfName string, vfID int, mac string) error {
	ifNames, _ := GetVFLinkNames(pfName, vfID)
	ifName := ifNames[0]

	vfLink, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("Netlink.LinkByName failed to lookup master %q: pfname %q vf %d %v", ifName, pfName, vfID, err)
	}

	hwAddr, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("Failed to parse mac address %s due to: %v", mac, err)
	}

	err = netlink.LinkSetHardwareAddr(vfLink, hwAddr)
	if err != nil {
		return fmt.Errorf("Failed to configure ifname %q pfname %q vf %d mac address to %q: %v", ifName, pfName, vfID, mac, err)
	}
	return nil
}
