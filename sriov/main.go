package main

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/containernetworking/cni/pkg/ipam"
	"github.com/containernetworking/cni/pkg/ns"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/intel/multus-cni/logging"
	"github.com/intel/sriov-cni/pkg/config"
	sriovtypes "github.com/intel/sriov-cni/pkg/types"
	"github.com/intel/sriov-cni/pkg/utils"
	"github.com/vishvananda/netlink"
)

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
	logging.SetLogFile("/var/log/sriov-cni.log")
	logging.SetLogLevel("debug")
}

func getPodName(args *skel.CmdArgs) string {
	podname := ""
	pairs := strings.Split(args.Args, ";")
	for _, pair := range pairs {
		kv := strings.Split(pair, "=")
		if len(kv) != 2 {
			return podname
		}
		if kv[0] == "K8S_POD_NAME" {
			podname = kv[1]
			break
		}
	}

	//if len(podname) == 0 {
	//	return 0, podname, fmt.Errorf("getVlanIndex: podname not found")
	//}
	return podname

}

func getVlanIndex(args *skel.CmdArgs) (int, string, error) {
	logging.Debugf("getVlanIndex args.Args %v", args.Args)

	podname := ""
	pairs := strings.Split(args.Args, ";")
	for _, pair := range pairs {
		kv := strings.Split(pair, "=")
		if len(kv) != 2 {
			return 0, podname, fmt.Errorf("getVlanIndex: invalid pair %q", pair)
		}
		if kv[0] == "K8S_POD_NAME" {
			podname = kv[1]
			break
		}
	}

	if len(podname) == 0 {
		return 0, podname, fmt.Errorf("getVlanIndex: podname not found")
	}

	ss := strings.Split(podname, "-")
	s := ss[len(ss)-1]

	i, err := strconv.Atoi(s)
	if err != nil {
		return 0, podname, fmt.Errorf("SRIOV-CNI failed to get vlanindex podname %q: %v", podname, err)
	}
	return i, podname, nil
}

func cmdAddBondedDevice(args *skel.CmdArgs, n *sriovtypes.NetConf) error {
	netns, err := ns.GetNS(args.Netns)
	// fill in DpdkConf from DeviceInfo
	if n.DPDKMode {
		n.DPDKConf.PCIaddr = n.DeviceInfo.PCIaddr
		n.DPDKConf.Ifname = args.IfName
		n.DPDKConf.VFID = n.DeviceInfo.Vfid
	}

	logging.Debugf("PKKK-X cmdAddBondedDevice DeviceID %s", n.DeviceID)

	if n.DeviceInfo != nil && n.DeviceInfo.PCIaddr != "" && n.DeviceInfo.Vfid >= 0 && n.DeviceInfo.Pfname != "" {
		err = setupVF(n, args.IfName, args.ContainerID, netns)
		defer func() {
			if err != nil {
				if !n.DPDKMode {
					err = netns.Do(func(_ ns.NetNS) error {
						_, err := netlink.LinkByName(args.IfName)
						return err
					})
				}
				if n.DPDKMode || err == nil {
					releaseVF(n, args.IfName, args.ContainerID, netns)
				}
			}
		}()
		if err != nil {
			return fmt.Errorf("failed to set up pod interface %q from the device %q: %v", args.IfName, n.Master, err)
		}
	} else {
		return fmt.Errorf("VF information are not available to invoke setupVF()")
	}

	return nil
}

func cmdAdd(args *skel.CmdArgs) error {
	n, err := config.LoadConf(args.StdinData)
	if err != nil {
		return fmt.Errorf("SRIOV-CNI failed to load netconf: %v", err)
	}

	podname := getPodName(args)
	logging.Debugf("PKKK-B cmdAdd podname %s ifname %s", podname, args.IfName)

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	defer netns.Close()

	if n.Vlans != nil {
		index, podname, err := getVlanIndex(args)
		if err != nil {
			return fmt.Errorf("failed to get VLAN index for args %v err:%v", args, n.Vlans, err)
		}
		if index < len(n.Vlans) {
			n.Vlan = n.Vlans[index]
			logging.Debugf("setupVF podname : %s, podifname %s, vlan %v vlans %v", podname, args.IfName, n.Vlan, n.Vlans)
		}
	}

	// Try assigning a VF from PF
	if n.DeviceInfo == nil && n.Master != "" {
		// Populate device info from PF
		if err := config.AssignFreeVF(n); err != nil {
			return fmt.Errorf("unable to get VF information %+v", err)
		}
	}

	if n.Sharedvf && !n.L2Mode {
		return fmt.Errorf("l2enable mode must be true to use shared net interface %q", n.Master)
	}

	err = cmdAddBondedDevice(args, n)
	if err != nil {
		return fmt.Errorf("failed to add bonded device: %v", err)
	}

	netlinkExpected, err := utils.ShouldHaveNetlink(n.Master, n.DeviceInfo.Vfid)
	if err != nil {
		return fmt.Errorf("failed to determine if interface should have netlink device: %v", err)
	}

	// skip the IPAM allocation for the DPDK and L2 mode
	var result *types.Result
	if n.DPDKMode || n.L2Mode || !netlinkExpected {
		logging.Debugf("PKKK-E cmdAdd success podname %s ifname %s", podname, args.IfName)
		return result.Print()
	}

	logging.Debugf("PKKK-E cmdAdd shouldn't be here podname %s ifname %s", podname, args.IfName)

	// run the IPAM plugin and get back the config to apply
	result, err = ipam.ExecAdd(n.IPAM.Type, args.StdinData)
	if err != nil {
		return fmt.Errorf("failed to set up IPAM plugin type %q from the device %q: %v", n.IPAM.Type, n.Master, err)
	}

	if result.IP4 == nil {
		return errors.New("IPAM plugin returned missing IPv4 config")
	}

	defer func() {
		if err != nil {
			ipam.ExecDel(n.IPAM.Type, args.StdinData)
		}
	}()

	err = netns.Do(func(_ ns.NetNS) error {
		return ipam.ConfigureIface(args.IfName, result)
	})
	if err != nil {
		return err
	}

	result.DNS = n.DNS
	return result.Print()
}

func cmdDel(args *skel.CmdArgs) error {
	n, err := config.LoadConf(args.StdinData)
	if err != nil {
		return err
	}

	podname := getPodName(args)
	logging.Debugf("PKKK-B cmdDel podname %s ifname %s", podname, args.IfName)

	// skip the IPAM release for the DPDK and L2 mode
	if !n.DPDKMode && !n.L2Mode && n.IPAM.Type != "" {
		err = ipam.ExecDel(n.IPAM.Type, args.StdinData)
		if err != nil {
			return err
		}
	}

	if args.Netns == "" {
		logging.Debugf("cmdDel return podname %s ifname %s", podname, args.IfName)
		return nil
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		// according to:
		// https://github.com/kubernetes/kubernetes/issues/43014#issuecomment-287164444
		// if provided path does not exist (e.x. when node was restarted)
		// plugin should silently return with success after releasing
		// IPAM resources
		_, ok := err.(ns.NSPathNotExistErr)
		if ok {
			return nil
		}

		logging.Debugf("cmdDel error1 podname %s ifname %s", podname, args.IfName)
		return fmt.Errorf("failed to open netns %q %v", netns, err)
	}
	defer netns.Close()

	if err = releaseVF(n, args.IfName, args.ContainerID, netns); err != nil {
		logging.Debugf("cmdDel releaseVF error1 podname %s ifname %s", podname, args.IfName)
		return err
	}

	logging.Debugf("PKKK-E cmdDel success podname %s ifname %s", podname, args.IfName)
	return nil
}

func main() {
	//skel.PluginMain(cmdAdd, cmdDel, version.Legacy)
	skel.PluginMain(cmdAdd, cmdDel, version.PluginSupports("0.3.0", "0.3.0"))
}
