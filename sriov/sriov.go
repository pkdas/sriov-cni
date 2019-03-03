package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/ns"
	"github.com/intel/multus-cni/logging"
	"github.com/intel/sriov-cni/pkg/config"
	"github.com/intel/sriov-cni/pkg/dpdk"
	sriovtypes "github.com/intel/sriov-cni/pkg/types"
	"github.com/intel/sriov-cni/pkg/utils"
	"github.com/vishvananda/netlink"
)

/*
 Link names given as os.FileInfo need to be sorted by their Index
*/

// LinksByIndex holds network interfaces name
type LinksByIndex []string

// LinksByIndex implements sort.Inteface
func (l LinksByIndex) Len() int { return len(l) }

// Swap implements Swap() method of sort interface
func (l LinksByIndex) Swap(i, j int) { l[i], l[j] = l[j], l[i] }

// Less implements Less() method of sort interface
func (l LinksByIndex) Less(i, j int) bool {
	linkA, _ := netlink.LinkByName(l[i])
	linkB, _ := netlink.LinkByName(l[j])

	return linkA.Attrs().Index < linkB.Attrs().Index
}

func setSharedVfVlan(ifName string, vfIdx int, vlan int) error {
	var err error
	var sharedifName string

	logging.Debugf("ERROR: setSharedVfVlan should not be called")

	vfDir := fmt.Sprintf("/sys/class/net/%s/device/net", ifName)
	if _, err := os.Lstat(vfDir); err != nil {
		return fmt.Errorf("failed to open the net dir of the device %q: %v", ifName, err)
	}

	infos, err := ioutil.ReadDir(vfDir)
	if err != nil {
		return fmt.Errorf("failed to read the net dir of the device %q: %v", ifName, err)
	}

	if len(infos) != config.MaxSharedVf {
		return fmt.Errorf("Given PF - %q is not having shared VF", ifName)
	}

	for _, dir := range infos {
		if strings.Compare(ifName, dir.Name()) != 0 {
			sharedifName = dir.Name()
		}
	}

	if sharedifName == "" {
		return fmt.Errorf("Shared ifname can't be empty")
	}

	iflink, err := netlink.LinkByName(sharedifName)
	if err != nil {
		return fmt.Errorf("failed to lookup the shared ifname %q: %v", sharedifName, err)
	}

	if err := netlink.LinkSetVfVlan(iflink, vfIdx, vlan); err != nil {
		return fmt.Errorf("failed to set vf %d vlan: %v for shared ifname %q", vfIdx, err, sharedifName)
	}

	return nil
}

func moveIfToNetns(ifname string, netns ns.NetNS) (string, error) {
	vfDev, err := netlink.LinkByName(ifname)
	if err != nil {
		logging.Debugf("moveIfToNetns error netlink.LinkByName has failed %s %v", ifname, err)
		return ifname, fmt.Errorf("failed to lookup vf device %v: %q", ifname, err)
	}

	if err = netlink.LinkSetDown(vfDev); err != nil {
		logging.Debugf("moveIfToNetns error netlink.LinkSetDown has failed %s %v", ifname, err)
		return ifname, fmt.Errorf("failed to down vf device %q: %v", ifname, err)
	}
	index := vfDev.Attrs().Index
	vfName := fmt.Sprintf("dev%d", index)
	if renameLink(ifname, vfName); err != nil {
		logging.Debugf("moveIfToNetns error renameLink has failed %s %s %v", ifname, vfName, err)
		return ifname, fmt.Errorf("failed to rename vf device %q to %q: %v", ifname, vfName, err)
	}

	if err = netlink.LinkSetUp(vfDev); err != nil {
		logging.Debugf("moveIfToNetns error netlink.LinkSetUp has failed %v %s %v", vfDev, ifname, err)
		return vfName, fmt.Errorf("failed to setup netlink device %v %q", ifname, err)
	}

	// move VF device to ns
	if err = netlink.LinkSetNsFd(vfDev, int(netns.Fd())); err != nil {
		logging.Debugf("moveIfToNetns error netlink.LinkSetNsFd has failed %v %s %v", vfDev, ifname, err)
		return vfName, fmt.Errorf("failed to move device %+v to netns: %q", ifname, err)
	}

	logging.Debugf("moveIfToNetns vfdev %v ifname %s ns %v", vfDev, ifname, netns)

	return vfName, nil
}

func setupVF(conf *sriovtypes.NetConf, podifName string, cid string, netns ns.NetNS) error {
	m, err := netlink.LinkByName(conf.Master)
	if err != nil {
		return fmt.Errorf("failed to lookup master %q: %v", conf.Master, err)
	}

	netlinkExpected, err := utils.ShouldHaveNetlink(conf.Master, conf.DeviceInfo.Vfid)
	if err != nil {
		return fmt.Errorf("failed to determine if interface should have netlink device: %v", err)
	}
	if !netlinkExpected {
		return nil
	}

	vfLinks, err := utils.GetVFLinkNames(conf.Master, conf.DeviceInfo.Vfid)
	if err != nil {
		return err
	}

	if conf.Vlan != 0 {
		if err = netlink.LinkSetVfVlan(m, conf.DeviceInfo.Vfid, conf.Vlan); err != nil {
			return fmt.Errorf("failed to set vf %d vlan: %v", conf.DeviceInfo.Vfid, err)
		}

		if conf.Sharedvf {
			if err = setSharedVfVlan(conf.Master, conf.DeviceInfo.Vfid, conf.Vlan); err != nil {
				return fmt.Errorf("failed to set shared vf %d vlan: %v", conf.DeviceInfo.Vfid, err)
			}
		}
	}

	logging.Debugf("setupVF start cid : %s, podifname %s, ns %v", cid, podifName, netns)
	logging.Debugf("setupVF master %s, vf %d pf %s pcie %s ", conf.Master, conf.DeviceInfo.Vfid, conf.DeviceInfo.Pfname, conf.DeviceInfo.PCIaddr)
	logging.Debugf("setupVF DPDK %t L2 %t Vlan %d deviceId %s", conf.DPDKMode, conf.L2Mode, conf.Vlan, conf.DeviceID)

	// /sys/class/net/enp59s0/device/device check for 0x1017 ConnectX5 - then ignore DPDK conf and always bind to kernel driver
	// /sys/class/net/enp59s0/device/driver -> ../../../../bus/pci/drivers/mlx5_core  - points to mlx5_core
	if conf.DPDKMode {
		if err = dpdk.SaveDpdkConf(cid, conf.CNIDir, conf.DPDKConf); err != nil {
			logging.Debugf("setupVF dpdk.SaveDpdkConf failed %v", err)
			return err
		}
		logging.Debugf("setupVF binding DPDK")
		rc := dpdk.Enabledpdkmode(conf.DPDKConf, vfLinks[0], true)
		logging.Debugf("setupVF DPDK complete - cid : %s, podifname %s, ns %v", cid, podifName, netns)
		return rc
	}

	// Sort links name if there are 2 or more PF links found for a VF;
	if len(vfLinks) > 1 {
		// sort Links FileInfo by their Link indices
		sort.Sort(LinksByIndex(vfLinks))
	}

	for i := 0; i < len(vfLinks); i++ {
		linkName := vfLinks[i]

		newLinkName, err := moveIfToNetns(linkName, netns)
		if err != nil {
			return err
		}
		vfLinks[i] = newLinkName
	}

	return netns.Do(func(_ ns.NetNS) error {

		ifName := podifName
		for i := 0; i < len(vfLinks); i++ {
			if len(vfLinks) == config.MaxSharedVf && i == (len(vfLinks)-1) {
				ifName = podifName + fmt.Sprintf("d%d", i)
			}

			err := renameLink(vfLinks[i], ifName)
			if err != nil {
				logging.Debugf("setupVF renameLink failed %v", err)
				return fmt.Errorf("failed to rename vf %d of the device %q to %q: %v", conf.DeviceInfo.Vfid, vfLinks[i], ifName, err)
			}

			// for L2 mode enable the pod net interface
			if conf.L2Mode != false {
				err = setUpLink(ifName)
				if err != nil {
					logging.Debugf("setupVF setUpLink failed %v", err)
					return fmt.Errorf("failed to set up the pod interface name %q: %v", ifName, err)
				}
			}
		}
		logging.Debugf("setupVF complete - cid : %s, podifname %s, ns %v", cid, podifName, netns)
		return nil
	})
}

func releaseVF(conf *sriovtypes.NetConf, podifName string, cid string, netns ns.NetNS) error {
	// check for the DPDK mode and release the allocated DPDK resources
	logging.Debugf("releaseVF start cid : %s, podifname %s, ns %v", cid, podifName, netns)
	logging.Debugf("releaseVF master %s, vf %d pf %s pcie %s ", conf.Master, conf.DeviceInfo.Vfid, conf.DeviceInfo.Pfname, conf.DeviceInfo.PCIaddr)
	logging.Debugf("releaseVF DPDK %t L2 %t Vlan %d deviceId %s", conf.DPDKMode, conf.L2Mode, conf.Vlan, conf.DeviceID)
	if conf.DPDKMode != false {
		// get the DPDK net conf in cniDir
		df, err := dpdk.GetConf(cid, podifName, conf.CNIDir)
		if err != nil {
			logging.Debugf("releaseVF dpdk.GetConf failed %v", err)
			return err
		}

		logging.Debugf("releaseVF unbind dpdk : pcieaddr %s ifname %s, kdriver %s dpdkdriver %s dpdktool %s vfid %d", df.PCIaddr, df.Ifname, df.KDriver, df.DPDKDriver, df.DPDKtool, df.VFID)

		// bind the sriov vf to the kernel driver
		if err := dpdk.Enabledpdkmode(df, df.Ifname, false); err != nil {
			logging.Debugf("releaseVF dpdk.Enabledpdkmode failed %v", err)
			return fmt.Errorf("DPDK: failed to bind %s to kernel space: %s", df.Ifname, err)
		}

		// PK FIX ME
		// unbinding from DPDK and binding to kernel driver takes a few seconds
		// the VLAN resetting call below is failing for i40e which takes couple of seconds
		time.Sleep(2 * time.Second)

		// reset vlan for DPDK code here
		pfLink, err := netlink.LinkByName(conf.Master)
		if err != nil {
			logging.Debugf("releaseVF netlink.LinkByName failed name %s error %v", conf.Master, err)
			return fmt.Errorf("DPDK: master device %s not found: %v", conf.Master, err)
		}

		if err = netlink.LinkSetVfVlan(pfLink, df.VFID, 0); err != nil {
			logging.Debugf("releaseVF netlink.LinkSetVfVlan failed vf %d error %v", df.VFID, err)
			return fmt.Errorf("DPDK: failed to reset vlan tag for vf %d: %v", df.VFID, err)
		}
		logging.Debugf("releaseVF in DPDKMode is complete")
		return nil
	}

	netlinkExpected, err := utils.ShouldHaveNetlink(conf.Master, conf.DeviceInfo.Vfid)
	if err != nil {
		return fmt.Errorf("failed to determine if interface should have netlink device: %v", err)
	}
	if !netlinkExpected {
		return nil
	}

	initns, err := ns.GetCurrentNS()
	if err != nil {
		logging.Debugf("releaseVF ns.GetCurrentNS failed error %v", err)
		return fmt.Errorf("failed to get init netns: %v", err)
	}

	if err = netns.Set(); err != nil {
		logging.Debugf("releaseVF netns.Set failed error %v", err)
		return fmt.Errorf("failed to enter netns %q: %v", netns, err)
	}

	if conf.L2Mode != false {
		//check for the shared vf net interface
		ifName := podifName + "d1"
		_, err := netlink.LinkByName(ifName)
		if err != nil {
			//logging.Debugf("releaseVF netlink.LinkByname failed not shared ifname %s %v", ifName, err)
			//return fmt.Errorf("unable to get shared PF device: %v", err)
			conf.Sharedvf = false
		} else {
			logging.Debugf("releaseVF netlink.LinkByname true and is shared ifname %s %v", ifName, err)
			conf.Sharedvf = true
		}
	}

	for i := 1; i <= config.MaxSharedVf; i++ {
		ifName := podifName
		pfName := conf.Master
		if i == config.MaxSharedVf {
			ifName = podifName + fmt.Sprintf("d%d", i-1)
			pfName, err = utils.GetSharedPF(conf.Master)
			if err != nil {
				logging.Debugf("releaseVF utils.GetSharedPF error shouldn't be here ifname %s %v", conf.Master, err)
				return fmt.Errorf("failed to look up shared PF device: %v", err)
			}
		}

		// get VF device
		vfDev, err := netlink.LinkByName(ifName)
		if err != nil {
			logging.Debugf("releaseVF netlink.LinkByName error ifname %s %v", conf.Master, err)
			return fmt.Errorf("failed to lookup vf device %q: %v", ifName, err)
		}

		// device name in init netns
		index := vfDev.Attrs().Index
		devName := fmt.Sprintf("dev%d", index)

		// shutdown VF device
		if err = netlink.LinkSetDown(vfDev); err != nil {
			logging.Debugf("releaseVF netlink.LinkSetDown error ifname %s %v %v", ifName, vfDev, err)
			return fmt.Errorf("failed to down vf device %q: %v", ifName, err)
		}

		// rename VF device
		err = renameLink(ifName, devName)
		if err != nil {
			logging.Debugf("releaseVF renameLink error ifname %s %s %v", ifName, devName, err)
			return fmt.Errorf("failed to rename vf device %q to %q: %v", ifName, devName, err)
		}

		// move VF device to init netns
		if err = netlink.LinkSetNsFd(vfDev, int(initns.Fd())); err != nil {
			logging.Debugf("releaseVF netlink.LinkSetNsFd  error ifname %s %v %v", ifName, vfDev, err)
			return fmt.Errorf("failed to move vf device %q to init netns: %v", ifName, err)
		}

		// reset vlan
		if conf.Vlan != 0 {
			err = initns.Do(func(_ ns.NetNS) error {
				return resetVfVlan(pfName, devName)
			})
			if err != nil {
				logging.Debugf("releaseVF resetVfVlan  error ifname %s %v %v", pfName, devName, err)
				return fmt.Errorf("failed to reset vlan: %v", err)
			}
		}

		//break the loop, if the namespace has no shared vf net interface
		if conf.Sharedvf != true {
			break
		}
	}

	logging.Debugf("releaseVF in DPDKMode is complete")
	return nil
}

func resetVfVlan(pfName, vfName string) error {

	// get the ifname sriov vf num
	vfTotal, err := utils.GetSriovNumVfs(pfName)
	if err != nil {
		return err
	}

	if vfTotal <= 0 {
		logging.Debugf("resetVfVlan, no virtual function in the device: %v", pfName)
		return fmt.Errorf("no virtual function in the device: %v", pfName)
	}

	// Get VF id
	var vf int
	idFound := false
	for vf = 0; vf < vfTotal; vf++ {
		vfDir := fmt.Sprintf("/sys/class/net/%s/device/virtfn%d/net/%s", pfName, vf, vfName)
		if _, err := os.Stat(vfDir); !os.IsNotExist(err) {
			idFound = true
			break
		}
	}

	if !idFound {
		logging.Debugf("resetVfVlan failed to get VF id for %s", vfName)
		return fmt.Errorf("failed to get VF id for %s", vfName)
	}

	pfLink, err := netlink.LinkByName(pfName)
	if err != nil {
		logging.Debugf("resetVfVlan failed master device %s not found", pfName)
		return fmt.Errorf("master device %s not found", pfName)
	}

	if err = netlink.LinkSetVfVlan(pfLink, vf, 0); err != nil {
		logging.Debugf("resetVfVlan failed in netlink.LinkSetVfVlan %s vf %d", pfName, vf)
		return fmt.Errorf("failed to reset vlan tag for vf %d: %v", vf, err)
	}

	logging.Debugf("vlan reset pfName %s vfName %s", pfName, vfName)

	return nil
}

func renameLink(curName, newName string) error {
	link, err := netlink.LinkByName(curName)
	if err != nil {
		logging.Debugf("renameLink failed in netlink.LinkByName %q: %v", curName, err)
		return fmt.Errorf("failed to lookup device %q: %v", curName, err)
	}

	//return netlink.LinkSetName(link, newName)
	err = netlink.LinkSetName(link, newName)
	if err != nil {
		logging.Debugf("renameLink failed in netlink.LinkSetName curName %q newName %s %v", curName, newName, err)
	}

	logging.Debugf("renamed link curName %s newName %s", curName, newName)

	return err
}

func setUpLink(ifName string) error {
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		logging.Debugf("setUpLink failed in netlink.LinkByName %q: %v", ifName, err)
		return fmt.Errorf("failed to set up device %q: %v", ifName, err)
	}

	err = netlink.LinkSetUp(link)
	if err != nil {
		logging.Debugf("setUpLink failed in netlink.LinkSetUp ifname %s %v", ifName, err)
	}
	return err
}
