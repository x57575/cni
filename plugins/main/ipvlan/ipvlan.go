// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/appc/cni/Godeps/_workspace/src/github.com/vishvananda/netlink"
	"github.com/appc/cni/pkg/ip"
	"github.com/appc/cni/pkg/ns"
	"github.com/appc/cni/pkg/plugin"
	"github.com/appc/cni/pkg/skel"
)

type NetConf struct {
	plugin.NetConf
	Master string `json:"master"`
	Mode   string `json:"mode"`
	IPMasq bool   `json:"ipMasq"`
	MTU    int    `json:"mtu"`
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func loadConf(bytes []byte) (*NetConf, error) {
	n := &NetConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}
	if n.Master == "" {
		return nil, fmt.Errorf(`"master" field is required. It specifies the host interface name to virtualize`)
	}
	return n, nil
}

func modeFromString(s string) (netlink.IPVlanMode, error) {
	switch s {
	case "":
		return netlink.IPVLAN_MODE_L2, nil
	case "l2":
		return netlink.IPVLAN_MODE_L2, nil
	case "l3":
		return netlink.IPVLAN_MODE_L3, nil
	default:
		return 0, fmt.Errorf("unknown ipvlan mode: %q", s)
	}
}

func createIpvlan(conf *NetConf, ifName string, netns *os.File) error {
	mode, err := modeFromString(conf.Mode)
	if err != nil {
		return err
	}

	m, err := netlink.LinkByName(conf.Master)
	if err != nil {
		return fmt.Errorf("failed to lookup master %q: %v", conf.Master, err)
	}

	mv := &netlink.IPVlan{
		LinkAttrs: netlink.LinkAttrs{
			MTU:         conf.MTU,
			Name:        ifName,
			ParentIndex: m.Attrs().Index,
			Namespace:   netlink.NsFd(int(netns.Fd())),
		},
		Mode: mode,
	}

	if err := netlink.LinkAdd(mv); err != nil {
		return fmt.Errorf("failed to create ipvlan: %v", err)
	}

	return err
}

func cmdAdd(args *skel.CmdArgs) error {
	n, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	netns, err := os.Open(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	defer netns.Close()

	tmpName, err := ip.RandomVethName()
	if err != nil {
		return err
	}

	if err = createIpvlan(n, tmpName, netns); err != nil {
		return err
	}

	// run the IPAM plugin and get back the config to apply
	result, err := plugin.ExecAdd(n.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}
	if result.IP4 == nil {
		return errors.New("IPAM plugin returned missing IPv4 config")
	}

	err = ns.WithNetNS(netns, func(_ *os.File) error {
		err := renameLink(tmpName, args.IfName)
		if err != nil {
			return fmt.Errorf("failed to rename ipvlan to %q: %v", args.IfName, err)
		}

		return plugin.ConfigureIface(args.IfName, result)
	})
	if err != nil {
		return err
	}

	if n.IPMasq {
		chain := "CNI-" + n.Name
		if err = ip.SetupIPMasq(ip.Network(&result.IP4.IP), chain); err != nil {
			return err
		}
	}

	return plugin.PrintResult(result)
}

func cmdDel(args *skel.CmdArgs) error {
	n, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	err = plugin.ExecDel(n.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}

	return ns.WithNetNSPath(args.Netns, func(hostNS *os.File) error {
		return ip.DelLinkByName(args.IfName)
	})
}

func renameLink(curName, newName string) error {
	link, err := netlink.LinkByName(curName)
	if err != nil {
		return err
	}

	return netlink.LinkSetName(link, newName)
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel)
}
