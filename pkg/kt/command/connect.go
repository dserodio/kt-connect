package command

import (
	"fmt"
	"github.com/alibaba/kt-connect/pkg/kt/registry"
	"github.com/cilium/ipam/service/allocator"
	"net"
	"os"
	"strings"

	"github.com/alibaba/kt-connect/pkg/common"
	"github.com/alibaba/kt-connect/pkg/kt/cluster"

	"github.com/alibaba/kt-connect/pkg/kt"
	"github.com/alibaba/kt-connect/pkg/kt/options"
	"github.com/alibaba/kt-connect/pkg/kt/util"
	"github.com/cilium/ipam/service/ipallocator"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	urfave "github.com/urfave/cli"
)

// newConnectCommand return new connect command
func newConnectCommand(cli kt.CliInterface, options *options.DaemonOptions, action ActionInterface) urfave.Command {
	return urfave.Command{
		Name:  "connect",
		Usage: "connection to kubernetes cluster",
		Flags: ConnectActionFlag(options),
		Action: func(c *urfave.Context) error {
			if options.Debug {
				zerolog.SetGlobalLevel(zerolog.DebugLevel)
			}
			if err := CompleteOptions(options); err != nil {
				return err
			}
			if err := combineKubeOpts(options); err != nil {
				return err
			}
			return action.Connect(cli, options)
		},
	}
}

func CompleteOptions(options *options.DaemonOptions) error {
	if options.ConnectOptions.Method == common.ConnectMethodTun {
		srcIP, destIP, err := allocateTunIP(options.ConnectOptions.TunCidr)
		if err != nil {
			return err
		}
		options.ConnectOptions.SourceIP = srcIP
		options.ConnectOptions.DestIP = destIP
	}

	return nil
}

// Connect connect vpn to kubernetes cluster
func (action *Action) Connect(cli kt.CliInterface, options *options.DaemonOptions) (err error) {
	if util.IsDaemonRunning(options.RuntimeOptions.PidFile) {
		return fmt.Errorf("another connect process already running with %s, exiting", options.RuntimeOptions.PidFile)
	}
	ch := SetUpCloseHandler(cli, options, "connect")
	if err = connectToCluster(cli, options); err != nil {
		return
	}
	// watch background process, clean the workspace and exit if background process occur exception
	go func() {
		<-util.Interrupt()
		CleanupWorkspace(cli, options)
		os.Exit(0)
	}()
	s := <-ch
	log.Info().Msgf("Terminal signal is %s", s)
	return
}

func connectToCluster(cli kt.CliInterface, options *options.DaemonOptions) (err error) {

	pid, err := util.WritePidFile(options.RuntimeOptions.PidFile)
	if err != nil {
		return
	}
	log.Info().Msgf("Connect start at %d", pid)

	kubernetes, err := cli.Kubernetes()
	if err != nil {
		return
	}

	if util.IsWindows() || len(options.ConnectOptions.Dump2HostsNamespaces) > 0 {
		setupDump2Host(options, kubernetes)
	}
	if options.ConnectOptions.Method == common.ConnectMethodSocks {
		err = registry.SetGlobalProxy(options.ConnectOptions.SocksPort, &options.RuntimeOptions.ProxyConfig)
		if err != nil {
			log.Error().Msgf("Failed to setup global connect proxy: %s", err.Error())
		}
		err = registry.SetHttpProxyEnvironmentVariable(options.ConnectOptions.SocksPort, &options.RuntimeOptions.ProxyConfig)
		if err != nil {
			log.Error().Msgf("Failed to setup global http proxy: %s", err.Error())
		}
	}

	endPointIP, podName, credential, err := getOrCreateShadow(options, err, kubernetes)
	if err != nil {
		return
	}

	cidrs, err := kubernetes.ClusterCrids(options.Namespace, options.ConnectOptions)
	if err != nil {
		return
	}

	return cli.Shadow().Outbound(podName, endPointIP, credential, cidrs, cli.Exec())
}

func getOrCreateShadow(options *options.DaemonOptions, err error, kubernetes cluster.KubernetesInterface) (string, string, *util.SSHCredential, error) {
	workload := fmt.Sprintf("kt-connect-daemon-%s", strings.ToLower(util.RandomString(5)))
	if options.ConnectOptions.ShareShadow {
		workload = fmt.Sprintf("kt-connect-daemon-connect-shared")
	}

	annotations := make(map[string]string)
	endPointIP, podName, sshcm, credential, err := kubernetes.GetOrCreateShadow(workload, options, labels(workload, options), annotations, envs(options))
	if err != nil {
		return "", "", nil, err
	}

	// record shadow name will clean up terminal
	options.RuntimeOptions.Shadow = workload
	options.RuntimeOptions.SSHCM = sshcm

	return endPointIP, podName, credential, nil
}

func setupDump2Host(options *options.DaemonOptions, kubernetes cluster.KubernetesInterface) {
	hosts := kubernetes.ServiceHosts(options.Namespace)
	for k, v := range hosts {
		log.Info().Msgf("Service found: %s %s", k, v)
	}
	if len(options.ConnectOptions.Dump2HostsNamespaces) > 0 {
		for _, namespace := range options.ConnectOptions.Dump2HostsNamespaces {
			if namespace == options.Namespace {
				continue
			}
			log.Debug().Msgf("Search service in %s namespace...", namespace)
			singleHosts := kubernetes.ServiceHosts(namespace)
			for svc, ip := range singleHosts {
				if ip == "" || ip == "None" {
					continue
				}
				log.Info().Msgf("Service found: %s.%s %s", svc, namespace, ip)
				hosts[svc+"."+namespace] = ip
			}
		}
	}
	util.DumpHosts(hosts)
	options.RuntimeOptions.Dump2Host = true
}

func envs(options *options.DaemonOptions) map[string]string {
	envs := make(map[string]string)
	if options.ConnectOptions.LocalDomain != "" {
		envs[common.EnvVarLocalDomain] = options.ConnectOptions.LocalDomain
	}
	if options.ConnectOptions.Method == common.ConnectMethodTun {
		envs[common.ClientTunIP] = options.ConnectOptions.SourceIP
		envs[common.ServerTunIP] = options.ConnectOptions.DestIP
	}
	return envs
}

func labels(workload string, options *options.DaemonOptions) map[string]string {
	labels := map[string]string{
		common.ControlBy:   common.KubernetesTool,
		common.KTComponent: common.ComponentConnect,
		common.KTName:      workload,
	}
	for k, v := range util.String2Map(options.Labels) {
		labels[k] = v
	}
	splits := strings.Split(workload, "-")
	labels[common.KTVersion] = splits[len(splits)-1]
	return labels
}

func allocateTunIP(cidr string) (srcIP, destIP string, err error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", err
	}
	rge, err := ipallocator.NewAllocatorCIDRRange(ipnet, func(max int, rangeSpec string) (allocator.Interface, error) {
		return allocator.NewContiguousAllocationMap(max, rangeSpec), nil
	})
	if err != nil {
		return "", "", err
	}
	ip1, err := rge.AllocateNext()
	if err != nil {
		return "", "", err
	}
	ip2, err := rge.AllocateNext()
	if err != nil {
		return "", "", err
	}
	return ip1.String(), ip2.String(), nil
}
