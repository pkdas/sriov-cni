package main

import (
	//"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	//"github.com/containernetworking/cni/pkg/ipam"
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

func cmdAddBondedDevice(args *skel.CmdArgs, n *sriovtypes.NetConf, ifname string) error {
	netns, err := ns.GetNS(args.Netns)

	logging.Debugf("PKKK-X cmdAddBondedDevice DeviceID %s ifname %s", n.DeviceID, ifname)

	// fill in DpdkConf from DeviceInfo
	if n.DPDKMode {
		n.DPDKConf.PCIaddr = n.DeviceInfo.PCIaddr
		n.DPDKConf.Ifname = ifname
		n.DPDKConf.VFID = n.DeviceInfo.Vfid
	}

	if n.DeviceInfo != nil && n.DeviceInfo.PCIaddr != "" && n.DeviceInfo.Vfid >= 0 && n.DeviceInfo.Pfname != "" {
		err = setupVF(n, ifname, args.ContainerID, netns)
		if err != nil {
			logging.Debugf("PKKK-ERROR cmdAddBondedDevice DeviceID %s ifname %s", n.DeviceID, ifname)
		}

		defer func() {
			if err != nil {
				if !n.DPDKMode {
					err = netns.Do(func(_ ns.NetNS) error {
						_, err := netlink.LinkByName(ifname)
						logging.Debugf("PKKK-EX cmdAddBondedDevice DeviceID %s ifname %s", n.DeviceID, ifname)
						return err
					})
				}
				if n.DPDKMode || err == nil {
					logging.Debugf("PKKK-EX1 cmdAddBondedDevice DeviceID %s ifname %s", n.DeviceID, ifname)
					releaseVF(n, ifname, args.ContainerID, netns)
				}
			}
		}()
		if err != nil {
			logging.Debugf("PKKK-EX2 cmdAddBondedDevice DeviceID %s ifname %s", n.DeviceID, ifname)
			return fmt.Errorf("failed to set up pod interface %q from the device %q: %v", ifname, n.Master, err)
		}
	} else {
		logging.Debugf("PKKK-EX3 cmdAddBondedDevice DeviceID %s ifname %s", n.DeviceID, ifname)
		return fmt.Errorf("VF information are not available to invoke setupVF()")
	}

	return nil
}

func cmdAdd(args *skel.CmdArgs) error {
	var result *types.Result

	podname := getPodName(args)

	n, bondedlist, err := config.LoadConf(args.StdinData)
	if err != nil {
		logging.Debugf("PKKK-Error podname %s ifname %s LoadConf return error", podname, args.IfName)
		return result.Print()
		//return fmt.Errorf("SRIOV-CNI failed to load netconf: %v", err)
	}

	// PK - FIXME
	logging.Debugf("PKKK-MAIN podname %s ifname %s %+v", podname, args.IfName, bondedlist)
	//return result.Print()

	if bondedlist == nil {
		logging.Debugf("PKKK-Error podname %s ifname %s bondedlist is empty", podname, args.IfName)
		return result.Print()
	}

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
			for _, slave := range bondedlist {
				slave.Vlan = n.Vlans[index]
			}
		}
	}

	if bondedlist != nil {
		if len(bondedlist) > 1 {
			// we set the mac address of bonded interfaces to the first device
			firstSlave := bondedlist[0]
			hwAddress, err := utils.GetVFHwAddress(firstSlave.Master, firstSlave.DeviceInfo.Vfid)
			logging.Debugf("PKKK-XX set bond macaddress podname : %s, podifname %s, mac %v", podname, args.IfName, hwAddress)
			if err == nil {
				for _, slave := range bondedlist {
					utils.SetVFHwAddress(slave.Master, slave.DeviceInfo.Vfid, hwAddress)
					logging.Debugf("PKKK-B set bond macaddress podname : %s podifname %s master %s vf %d mac %v", podname, args.IfName, slave.Master, slave.DeviceInfo.Vfid, hwAddress)
				}
			}
		}

		for i, slave := range bondedlist {
			ifname := ""
			if len(bondedlist) == 1 {
				ifname = args.IfName
			} else {
				ifname = args.IfName + "-" + strconv.Itoa(i)
			}
			err = cmdAddBondedDevice(args, slave, ifname)
			if err != nil {
				logging.Debugf("PKKK-B cmdAddBondedDevice failed %v", ifname)
				return fmt.Errorf("failed to add bonded device: %v", err)
			}
			logging.Debugf("PKKK-B cmdAddBondedDevice success %v", ifname)
		}
	}

	// no error
	return result.Print()
}

func cmdDel(args *skel.CmdArgs) error {
	n, bondedlist, err := config.LoadConf(args.StdinData)
	if err != nil {
		return err
	}

	podname := getPodName(args)
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

	if bondedlist != nil {
		for i, slave := range bondedlist {
			ifname := ""
			if len(bondedlist) == 1 {
				ifname = args.IfName
			} else {
				ifname = args.IfName + "-" + strconv.Itoa(i)
			}
			if err = releaseVF(slave, ifname, args.ContainerID, netns); err != nil {
				logging.Debugf("cmdDel releaseVF error1 podname %s ifname %s", podname, ifname)
				return err
			}
			logging.Debugf("PKKK-E bondedlist cmdDel success podname %s ifname %s", podname, ifname)
		}
		if len(bondedlist) > 1 {
			hwAddress := "00:00:00:00:00:00"
			for _, slave := range bondedlist {
				utils.SetVFHwAddress(slave.Master, slave.DeviceInfo.Vfid, hwAddress)
				logging.Debugf("PKKK-B clearing bond macaddress podname : %s, podifname %s, vf %d mac %v", podname, args.IfName, slave.DeviceInfo.Vfid, hwAddress)
			}
		}
		logging.Debugf("PKKK-E bondedlist cmdDel success podname %s ifname %s", podname, args.IfName)
		return nil
	}

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
