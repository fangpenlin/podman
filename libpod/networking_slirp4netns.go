//go:build linux
// +build linux

package libpod

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containers/common/libnetwork/types"
	"github.com/containers/podman/v4/pkg/errorhandling"
	"github.com/containers/podman/v4/pkg/rootless"
	"github.com/containers/podman/v4/pkg/rootlessport"
	"github.com/containers/podman/v4/pkg/servicereaper"
	"github.com/sirupsen/logrus"
)

type slirpFeatures struct {
	HasDisableHostLoopback bool
	HasMTU                 bool
	HasEnableSandbox       bool
	HasEnableSeccomp       bool
	HasCIDR                bool
	HasOutboundAddr        bool
	HasIPv6                bool
}

type slirp4netnsCmdArg struct {
	Proto     string `json:"proto,omitempty"`
	HostAddr  string `json:"host_addr"`
	HostPort  uint16 `json:"host_port"`
	GuestAddr string `json:"guest_addr"`
	GuestPort uint16 `json:"guest_port"`
}

type slirp4netnsCmd struct {
	Execute string            `json:"execute"`
	Args    slirp4netnsCmdArg `json:"arguments"`
}

type slirp4netnsNetworkOptions struct {
	cidr                string
	disableHostLoopback bool
	enableIPv6          bool
	isSlirpHostForward  bool
	noPivotRoot         bool
	mtu                 int
	outboundAddr        string
	outboundAddr6       string
}

const (
	ipv6ConfDefaultAcceptDadSysctl = "/proc/sys/net/ipv6/conf/default/accept_dad"
	slirp4netnsBinaryName          = "slirp4netns"
)

func checkSlirpFlags(path string) (*slirpFeatures, error) {
	cmd := exec.Command(path, "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("slirp4netns %q: %w", out, err)
	}
	return &slirpFeatures{
		HasDisableHostLoopback: strings.Contains(string(out), "--disable-host-loopback"),
		HasMTU:                 strings.Contains(string(out), "--mtu"),
		HasEnableSandbox:       strings.Contains(string(out), "--enable-sandbox"),
		HasEnableSeccomp:       strings.Contains(string(out), "--enable-seccomp"),
		HasCIDR:                strings.Contains(string(out), "--cidr"),
		HasOutboundAddr:        strings.Contains(string(out), "--outbound-addr"),
		HasIPv6:                strings.Contains(string(out), "--enable-ipv6"),
	}, nil
}

func parseSlirp4netnsNetworkOptions(r *Runtime, extraOptions []string) (*slirp4netnsNetworkOptions, error) {
	slirpOptions := make([]string, 0, len(r.config.Engine.NetworkCmdOptions)+len(extraOptions))
	slirpOptions = append(slirpOptions, r.config.Engine.NetworkCmdOptions...)
	slirpOptions = append(slirpOptions, extraOptions...)
	slirp4netnsOpts := &slirp4netnsNetworkOptions{
		// overwrite defaults
		disableHostLoopback: true,
		mtu:                 slirp4netnsMTU,
		noPivotRoot:         r.config.Engine.NoPivotRoot,
		enableIPv6:          true,
	}
	for _, o := range slirpOptions {
		parts := strings.SplitN(o, "=", 2)
		if len(parts) < 2 {
			return nil, fmt.Errorf("unknown option for slirp4netns: %q", o)
		}
		option, value := parts[0], parts[1]
		switch option {
		case "cidr":
			ipv4, _, err := net.ParseCIDR(value)
			if err != nil || ipv4.To4() == nil {
				return nil, fmt.Errorf("invalid cidr %q", value)
			}
			slirp4netnsOpts.cidr = value
		case "port_handler":
			switch value {
			case "slirp4netns":
				slirp4netnsOpts.isSlirpHostForward = true
			case "rootlesskit":
				slirp4netnsOpts.isSlirpHostForward = false
			default:
				return nil, fmt.Errorf("unknown port_handler for slirp4netns: %q", value)
			}
		case "allow_host_loopback":
			switch value {
			case "true":
				slirp4netnsOpts.disableHostLoopback = false
			case "false":
				slirp4netnsOpts.disableHostLoopback = true
			default:
				return nil, fmt.Errorf("invalid value of allow_host_loopback for slirp4netns: %q", value)
			}
		case "enable_ipv6":
			switch value {
			case "true":
				slirp4netnsOpts.enableIPv6 = true
			case "false":
				slirp4netnsOpts.enableIPv6 = false
			default:
				return nil, fmt.Errorf("invalid value of enable_ipv6 for slirp4netns: %q", value)
			}
		case "outbound_addr":
			ipv4 := net.ParseIP(value)
			if ipv4 == nil || ipv4.To4() == nil {
				_, err := net.InterfaceByName(value)
				if err != nil {
					return nil, fmt.Errorf("invalid outbound_addr %q", value)
				}
			}
			slirp4netnsOpts.outboundAddr = value
		case "outbound_addr6":
			ipv6 := net.ParseIP(value)
			if ipv6 == nil || ipv6.To4() != nil {
				_, err := net.InterfaceByName(value)
				if err != nil {
					return nil, fmt.Errorf("invalid outbound_addr6: %q", value)
				}
			}
			slirp4netnsOpts.outboundAddr6 = value
		case "mtu":
			var err error
			slirp4netnsOpts.mtu, err = strconv.Atoi(value)
			if slirp4netnsOpts.mtu < 68 || err != nil {
				return nil, fmt.Errorf("invalid mtu %q", value)
			}
		default:
			return nil, fmt.Errorf("unknown option for slirp4netns: %q", o)
		}
	}
	return slirp4netnsOpts, nil
}

func createBasicSlirp4netnsCmdArgs(options *slirp4netnsNetworkOptions, features *slirpFeatures) ([]string, error) {
	cmdArgs := []string{}
	if options.disableHostLoopback && features.HasDisableHostLoopback {
		cmdArgs = append(cmdArgs, "--disable-host-loopback")
	}
	if options.mtu > -1 && features.HasMTU {
		cmdArgs = append(cmdArgs, fmt.Sprintf("--mtu=%d", options.mtu))
	}
	if !options.noPivotRoot && features.HasEnableSandbox {
		cmdArgs = append(cmdArgs, "--enable-sandbox")
	}
	if features.HasEnableSeccomp {
		cmdArgs = append(cmdArgs, "--enable-seccomp")
	}

	if options.cidr != "" {
		if !features.HasCIDR {
			return nil, fmt.Errorf("cidr not supported")
		}
		cmdArgs = append(cmdArgs, fmt.Sprintf("--cidr=%s", options.cidr))
	}

	if options.enableIPv6 {
		if !features.HasIPv6 {
			return nil, fmt.Errorf("enable_ipv6 not supported")
		}
		cmdArgs = append(cmdArgs, "--enable-ipv6")
	}

	if options.outboundAddr != "" {
		if !features.HasOutboundAddr {
			return nil, fmt.Errorf("outbound_addr not supported")
		}
		cmdArgs = append(cmdArgs, fmt.Sprintf("--outbound-addr=%s", options.outboundAddr))
	}

	if options.outboundAddr6 != "" {
		if !features.HasOutboundAddr || !features.HasIPv6 {
			return nil, fmt.Errorf("outbound_addr6 not supported")
		}
		if !options.enableIPv6 {
			return nil, fmt.Errorf("enable_ipv6=true is required for outbound_addr6")
		}
		cmdArgs = append(cmdArgs, fmt.Sprintf("--outbound-addr6=%s", options.outboundAddr6))
	}

	return cmdArgs, nil
}

// setupSlirp4netns can be called in rootful as well as in rootless
func (r *Runtime) setupSlirp4netns(ctr *Container, netns string) error {
	path := r.config.Engine.NetworkCmdPath
	if path == "" {
		var err error
		path, err = r.config.FindHelperBinary(slirp4netnsBinaryName, true)
		if err != nil {
			return fmt.Errorf("could not find slirp4netns, the network namespace can't be configured: %w", err)
		}
	}

	syncR, syncW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to open pipe: %w", err)
	}
	defer errorhandling.CloseQuiet(syncR)
	defer errorhandling.CloseQuiet(syncW)

	havePortMapping := len(ctr.config.PortMappings) > 0
	logPath := filepath.Join(ctr.runtime.config.Engine.TmpDir, fmt.Sprintf("slirp4netns-%s.log", ctr.config.ID))

	ctrNetworkSlipOpts := []string{}
	if ctr.config.NetworkOptions != nil {
		ctrNetworkSlipOpts = append(ctrNetworkSlipOpts, ctr.config.NetworkOptions[slirp4netnsBinaryName]...)
	}
	netOptions, err := parseSlirp4netnsNetworkOptions(r, ctrNetworkSlipOpts)
	if err != nil {
		return err
	}
	slirpFeatures, err := checkSlirpFlags(path)
	if err != nil {
		return fmt.Errorf("checking slirp4netns binary %s: %q: %w", path, err, err)
	}
	cmdArgs, err := createBasicSlirp4netnsCmdArgs(netOptions, slirpFeatures)
	if err != nil {
		return err
	}

	// the slirp4netns arguments being passed are described as follows:
	// from the slirp4netns documentation: https://github.com/rootless-containers/slirp4netns
	// -c, --configure Brings up the tap interface
	// -e, --exit-fd=FD specify the FD for terminating slirp4netns
	// -r, --ready-fd=FD specify the FD to write to when the initialization steps are finished
	cmdArgs = append(cmdArgs, "-c", "-e", "3", "-r", "4")

	var apiSocket string
	if havePortMapping && netOptions.isSlirpHostForward {
		apiSocket = filepath.Join(ctr.runtime.config.Engine.TmpDir, fmt.Sprintf("%s.net", ctr.config.ID))
		cmdArgs = append(cmdArgs, "--api-socket", apiSocket)
	}
	netnsPath := ""
	if !ctr.config.PostConfigureNetNS {
		ctr.rootlessSlirpSyncR, ctr.rootlessSlirpSyncW, err = os.Pipe()
		if err != nil {
			return fmt.Errorf("failed to create rootless network sync pipe: %w", err)
		}
		netnsPath = netns
		cmdArgs = append(cmdArgs, "--netns-type=path", netnsPath, "tap0")
	} else {
		defer errorhandling.CloseQuiet(ctr.rootlessSlirpSyncR)
		defer errorhandling.CloseQuiet(ctr.rootlessSlirpSyncW)
		netnsPath = fmt.Sprintf("/proc/%d/ns/net", ctr.state.PID)
		// we don't use --netns-path here (unavailable for slirp4netns < v0.4)
		cmdArgs = append(cmdArgs, fmt.Sprintf("%d", ctr.state.PID), "tap0")
	}

	cmd := exec.Command(path, cmdArgs...)
	logrus.Debugf("slirp4netns command: %s", strings.Join(cmd.Args, " "))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// workaround for https://github.com/rootless-containers/slirp4netns/pull/153
	if !netOptions.noPivotRoot && slirpFeatures.HasEnableSandbox {
		cmd.SysProcAttr.Cloneflags = syscall.CLONE_NEWNS
		cmd.SysProcAttr.Unshareflags = syscall.CLONE_NEWNS
	}

	// Leak one end of the pipe in slirp4netns, the other will be sent to conmon
	cmd.ExtraFiles = append(cmd.ExtraFiles, ctr.rootlessSlirpSyncR, syncW)

	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("failed to open slirp4netns log file %s: %w", logPath, err)
	}
	defer logFile.Close()
	// Unlink immediately the file so we won't need to worry about cleaning it up later.
	// It is still accessible through the open fd logFile.
	if err := os.Remove(logPath); err != nil {
		return fmt.Errorf("delete file %s: %w", logPath, err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	var slirpReadyWg, netnsReadyWg *sync.WaitGroup
	if netOptions.enableIPv6 {
		// use two wait groups to make sure we set the sysctl before
		// starting slirp and reset it only after slirp is ready
		slirpReadyWg = &sync.WaitGroup{}
		netnsReadyWg = &sync.WaitGroup{}
		slirpReadyWg.Add(1)
		netnsReadyWg.Add(1)

		go func() {
			err := ns.WithNetNSPath(netnsPath, func(_ ns.NetNS) error {
				// Duplicate Address Detection slows the ipv6 setup down for 1-2 seconds.
				// Since slirp4netns is run in its own namespace and not directly routed
				// we can skip this to make the ipv6 address immediately available.
				// We change the default to make sure the slirp tap interface gets the
				// correct value assigned so DAD is disabled for it
				// Also make sure to change this value back to the original after slirp4netns
				// is ready in case users rely on this sysctl.
				orgValue, err := os.ReadFile(ipv6ConfDefaultAcceptDadSysctl)
				if err != nil {
					netnsReadyWg.Done()
					// on ipv6 disabled systems the sysctl does not exist
					// so we should not error
					if errors.Is(err, os.ErrNotExist) {
						return nil
					}
					return err
				}
				err = os.WriteFile(ipv6ConfDefaultAcceptDadSysctl, []byte("0"), 0644)
				netnsReadyWg.Done()
				if err != nil {
					return err
				}

				// wait until slirp4nets is ready before resetting this value
				slirpReadyWg.Wait()
				return os.WriteFile(ipv6ConfDefaultAcceptDadSysctl, orgValue, 0644)
			})
			if err != nil {
				logrus.Warnf("failed to set net.ipv6.conf.default.accept_dad sysctl: %v", err)
			}
		}()

		// wait until we set the sysctl
		netnsReadyWg.Wait()
	}

	if err := cmd.Start(); err != nil {
		if netOptions.enableIPv6 {
			slirpReadyWg.Done()
		}
		return fmt.Errorf("failed to start slirp4netns process: %w", err)
	}
	defer func() {
		servicereaper.AddPID(cmd.Process.Pid)
		if err := cmd.Process.Release(); err != nil {
			logrus.Errorf("Unable to release command process: %q", err)
		}
	}()

	err = waitForSync(syncR, cmd, logFile, 1*time.Second)
	if netOptions.enableIPv6 {
		slirpReadyWg.Done()
	}
	if err != nil {
		return err
	}

	// Set a default slirp subnet. Parsing a string with the net helper is easier than building the struct myself
	_, ctr.slirp4netnsSubnet, _ = net.ParseCIDR(defaultSlirp4netnsSubnet)

	// Set slirp4netnsSubnet addresses now that we are pretty sure the command executed
	if netOptions.cidr != "" {
		ipv4, ipv4network, err := net.ParseCIDR(netOptions.cidr)
		if err != nil || ipv4.To4() == nil {
			return fmt.Errorf("invalid cidr %q", netOptions.cidr)
		}
		ctr.slirp4netnsSubnet = ipv4network
	}

	if havePortMapping {
		if netOptions.isSlirpHostForward {
			return r.setupRootlessPortMappingViaSlirp(ctr, cmd, apiSocket)
		}
		return r.setupRootlessPortMappingViaRLK(ctr, netnsPath, nil)
	}

	return nil
}

// Get expected slirp ipv4 address based on subnet. If subnet is null use default subnet
// Reference: https://github.com/rootless-containers/slirp4netns/blob/master/slirp4netns.1.md#description
func GetSlirp4netnsIP(subnet *net.IPNet) (*net.IP, error) {
	_, slirpSubnet, _ := net.ParseCIDR(defaultSlirp4netnsSubnet)
	if subnet != nil {
		slirpSubnet = subnet
	}
	expectedIP, err := addToIP(slirpSubnet, uint32(100))
	if err != nil {
		return nil, fmt.Errorf("calculating expected ip for slirp4netns: %w", err)
	}
	return expectedIP, nil
}

// Get expected slirp Gateway ipv4 address based on subnet
// Reference: https://github.com/rootless-containers/slirp4netns/blob/master/slirp4netns.1.md#description
func GetSlirp4netnsGateway(subnet *net.IPNet) (*net.IP, error) {
	_, slirpSubnet, _ := net.ParseCIDR(defaultSlirp4netnsSubnet)
	if subnet != nil {
		slirpSubnet = subnet
	}
	expectedGatewayIP, err := addToIP(slirpSubnet, uint32(2))
	if err != nil {
		return nil, fmt.Errorf("calculating expected gateway ip for slirp4netns: %w", err)
	}
	return expectedGatewayIP, nil
}

// Get expected slirp DNS ipv4 address based on subnet
// Reference: https://github.com/rootless-containers/slirp4netns/blob/master/slirp4netns.1.md#description
func GetSlirp4netnsDNS(subnet *net.IPNet) (*net.IP, error) {
	_, slirpSubnet, _ := net.ParseCIDR(defaultSlirp4netnsSubnet)
	if subnet != nil {
		slirpSubnet = subnet
	}
	expectedDNSIP, err := addToIP(slirpSubnet, uint32(3))
	if err != nil {
		return nil, fmt.Errorf("calculating expected dns ip for slirp4netns: %w", err)
	}
	return expectedDNSIP, nil
}

// Helper function to calculate slirp ip address offsets
// Adapted from: https://github.com/signalsciences/ipv4/blob/master/int.go#L12-L24
func addToIP(subnet *net.IPNet, offset uint32) (*net.IP, error) {
	// I have no idea why I have to do this, but if I don't ip is 0
	ipFixed := subnet.IP.To4()

	ipInteger := uint32(ipFixed[3]) | uint32(ipFixed[2])<<8 | uint32(ipFixed[1])<<16 | uint32(ipFixed[0])<<24
	ipNewRaw := ipInteger + offset
	// Avoid overflows
	if ipNewRaw < ipInteger {
		return nil, fmt.Errorf("integer overflow while calculating ip address offset, %s + %d", ipFixed, offset)
	}
	ipNew := net.IPv4(byte(ipNewRaw>>24), byte(ipNewRaw>>16&0xFF), byte(ipNewRaw>>8)&0xFF, byte(ipNewRaw&0xFF))
	if !subnet.Contains(ipNew) {
		return nil, fmt.Errorf("calculated ip address %s is not within given subnet %s", ipNew.String(), subnet.String())
	}
	return &ipNew, nil
}

func waitForSync(syncR *os.File, cmd *exec.Cmd, logFile io.ReadSeeker, timeout time.Duration) error {
	prog := filepath.Base(cmd.Path)
	if len(cmd.Args) > 0 {
		prog = cmd.Args[0]
	}
	b := make([]byte, 16)
	for {
		if err := syncR.SetDeadline(time.Now().Add(timeout)); err != nil {
			return fmt.Errorf("setting %s pipe timeout: %w", prog, err)
		}
		// FIXME: return err as soon as proc exits, without waiting for timeout
		if _, err := syncR.Read(b); err == nil {
			break
		} else {
			if os.IsTimeout(err) {
				// Check if the process is still running.
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(cmd.Process.Pid, &status, syscall.WNOHANG, nil)
				if err != nil {
					return fmt.Errorf("failed to read %s process status: %w", prog, err)
				}
				if pid != cmd.Process.Pid {
					continue
				}
				if status.Exited() {
					// Seek at the beginning of the file and read all its content
					if _, err := logFile.Seek(0, 0); err != nil {
						logrus.Errorf("Could not seek log file: %q", err)
					}
					logContent, err := io.ReadAll(logFile)
					if err != nil {
						return fmt.Errorf("%s failed: %w", prog, err)
					}
					return fmt.Errorf("%s failed: %q", prog, logContent)
				}
				if status.Signaled() {
					return fmt.Errorf("%s killed by signal", prog)
				}
				continue
			}
			return fmt.Errorf("failed to read from %s sync pipe: %w", prog, err)
		}
	}
	return nil
}

func (r *Runtime) setupRootlessPortMappingViaRLK(ctr *Container, netnsPath string, netStatus map[string]types.StatusBlock) error {
	syncR, syncW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to open pipe: %w", err)
	}
	defer errorhandling.CloseQuiet(syncR)
	defer errorhandling.CloseQuiet(syncW)

	logPath := filepath.Join(ctr.runtime.config.Engine.TmpDir, fmt.Sprintf("rootlessport-%s.log", ctr.config.ID))
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("failed to open rootlessport log file %s: %w", logPath, err)
	}
	defer logFile.Close()
	// Unlink immediately the file so we won't need to worry about cleaning it up later.
	// It is still accessible through the open fd logFile.
	if err := os.Remove(logPath); err != nil {
		return fmt.Errorf("delete file %s: %w", logPath, err)
	}

	if !ctr.config.PostConfigureNetNS {
		ctr.rootlessPortSyncR, ctr.rootlessPortSyncW, err = os.Pipe()
		if err != nil {
			return fmt.Errorf("failed to create rootless port sync pipe: %w", err)
		}
	}

	childIP := getRootlessPortChildIP(ctr, netStatus)
	cfg := rootlessport.Config{
		Mappings:    ctr.convertPortMappings(),
		NetNSPath:   netnsPath,
		ExitFD:      3,
		ReadyFD:     4,
		TmpDir:      ctr.runtime.config.Engine.TmpDir,
		ChildIP:     childIP,
		ContainerID: ctr.config.ID,
		RootlessCNI: ctr.config.NetMode.IsBridge() && rootless.IsRootless(),
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	cfgR := bytes.NewReader(cfgJSON)
	var stdout bytes.Buffer
	path, err := r.config.FindHelperBinary(rootlessport.BinaryName, false)
	if err != nil {
		return err
	}
	cmd := exec.Command(path)
	cmd.Args = []string{rootlessport.BinaryName}

	// Leak one end of the pipe in rootlessport process, the other will be sent to conmon
	defer errorhandling.CloseQuiet(ctr.rootlessPortSyncR)

	cmd.ExtraFiles = append(cmd.ExtraFiles, ctr.rootlessPortSyncR, syncW)
	cmd.Stdin = cfgR
	// stdout is for human-readable error, stderr is for debug log
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(logFile, &logrusDebugWriter{"rootlessport: "})
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start rootlessport process: %w", err)
	}
	defer func() {
		servicereaper.AddPID(cmd.Process.Pid)
		if err := cmd.Process.Release(); err != nil {
			logrus.Errorf("Unable to release rootlessport process: %q", err)
		}
	}()
	if err := waitForSync(syncR, cmd, logFile, 3*time.Second); err != nil {
		stdoutStr := stdout.String()
		if stdoutStr != "" {
			// err contains full debug log and too verbose, so return stdoutStr
			logrus.Debug(err)
			return fmt.Errorf("rootlessport " + strings.TrimSuffix(stdoutStr, "\n"))
		}
		return err
	}
	logrus.Debug("rootlessport is ready")
	return nil
}

func (r *Runtime) setupRootlessPortMappingViaSlirp(ctr *Container, cmd *exec.Cmd, apiSocket string) (err error) {
	const pidWaitTimeout = 60 * time.Second
	chWait := make(chan error)
	go func() {
		interval := 25 * time.Millisecond
		for i := time.Duration(0); i < pidWaitTimeout; i += interval {
			// Check if the process is still running.
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(cmd.Process.Pid, &status, syscall.WNOHANG, nil)
			if err != nil {
				break
			}
			if pid != cmd.Process.Pid {
				continue
			}
			if status.Exited() || status.Signaled() {
				chWait <- fmt.Errorf("slirp4netns exited with status %d", status.ExitStatus())
			}
			time.Sleep(interval)
		}
	}()
	defer close(chWait)

	// wait that API socket file appears before trying to use it.
	if _, err := WaitForFile(apiSocket, chWait, pidWaitTimeout); err != nil {
		return fmt.Errorf("waiting for slirp4nets to create the api socket file %s: %w", apiSocket, err)
	}

	// for each port we want to add we need to open a connection to the slirp4netns control socket
	// and send the add_hostfwd command.
	for _, port := range ctr.convertPortMappings() {
		protocols := strings.Split(port.Protocol, ",")
		for _, protocol := range protocols {
			hostIP := port.HostIP
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}
			for i := uint16(0); i < port.Range; i++ {
				if err := openSlirp4netnsPort(apiSocket, protocol, hostIP, port.HostPort+i, port.ContainerPort+i); err != nil {
					return err
				}
			}
		}
	}
	logrus.Debug("slirp4netns port-forwarding setup via add_hostfwd is ready")
	return nil
}

// openSlirp4netnsPort sends the slirp4netns pai quey to the given socket
func openSlirp4netnsPort(apiSocket, proto, hostip string, hostport, guestport uint16) error {
	conn, err := net.Dial("unix", apiSocket)
	if err != nil {
		return fmt.Errorf("cannot open connection to %s: %w", apiSocket, err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			logrus.Errorf("Unable to close slirp4netns connection: %q", err)
		}
	}()
	apiCmd := slirp4netnsCmd{
		Execute: "add_hostfwd",
		Args: slirp4netnsCmdArg{
			Proto:     proto,
			HostAddr:  hostip,
			HostPort:  hostport,
			GuestPort: guestport,
		},
	}
	// create the JSON payload and send it.  Mark the end of request shutting down writes
	// to the socket, as requested by slirp4netns.
	data, err := json.Marshal(&apiCmd)
	if err != nil {
		return fmt.Errorf("cannot marshal JSON for slirp4netns: %w", err)
	}
	if _, err := conn.Write([]byte(fmt.Sprintf("%s\n", data))); err != nil {
		return fmt.Errorf("cannot write to control socket %s: %w", apiSocket, err)
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		return fmt.Errorf("cannot shutdown the socket %s: %w", apiSocket, err)
	}
	buf := make([]byte, 2048)
	readLength, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("cannot read from control socket %s: %w", apiSocket, err)
	}
	// if there is no 'error' key in the received JSON data, then the operation was
	// successful.
	var y map[string]interface{}
	if err := json.Unmarshal(buf[0:readLength], &y); err != nil {
		return fmt.Errorf("parsing error status from slirp4netns: %w", err)
	}
	if e, found := y["error"]; found {
		return fmt.Errorf("from slirp4netns while setting up port redirection: %v", e)
	}
	return nil
}

func getRootlessPortChildIP(c *Container, netStatus map[string]types.StatusBlock) string {
	if c.config.NetMode.IsSlirp4netns() {
		slirp4netnsIP, err := GetSlirp4netnsIP(c.slirp4netnsSubnet)
		if err != nil {
			return ""
		}
		return slirp4netnsIP.String()
	}

	var ipv6 net.IP
	for _, status := range netStatus {
		for _, netInt := range status.Interfaces {
			for _, netAddress := range netInt.Subnets {
				ipv4 := netAddress.IPNet.IP.To4()
				if ipv4 != nil {
					return ipv4.String()
				}
				ipv6 = netAddress.IPNet.IP
			}
		}
	}
	if ipv6 != nil {
		return ipv6.String()
	}
	return ""
}

// reloadRootlessRLKPortMapping will trigger a reload for the port mappings in the rootlessport process.
// This should only be called by network connect/disconnect and only as rootless.
func (c *Container) reloadRootlessRLKPortMapping() error {
	if len(c.config.PortMappings) == 0 {
		return nil
	}
	childIP := getRootlessPortChildIP(c, c.state.NetworkStatus)
	logrus.Debugf("reloading rootless ports for container %s, childIP is %s", c.config.ID, childIP)

	conn, err := openUnixSocket(filepath.Join(c.runtime.config.Engine.TmpDir, "rp", c.config.ID))
	if err != nil {
		return fmt.Errorf("could not reload rootless port mappings, port forwarding may no longer work correctly: %w", err)
	}
	defer conn.Close()
	enc := json.NewEncoder(conn)
	err = enc.Encode(childIP)
	if err != nil {
		return fmt.Errorf("port reloading failed: %w", err)
	}
	b, err := io.ReadAll(conn)
	if err != nil {
		return fmt.Errorf("port reloading failed: %w", err)
	}
	data := string(b)
	if data != "OK" {
		return fmt.Errorf("port reloading failed: %s", data)
	}
	return nil
}
