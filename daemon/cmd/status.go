// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-openapi/runtime/middleware"
	"github.com/go-openapi/strfmt"
	"github.com/sirupsen/logrus"
	versionapi "k8s.io/apimachinery/pkg/version"

	"github.com/cilium/cilium/api/v1/models"
	. "github.com/cilium/cilium/api/v1/server/restapi/daemon"
	"github.com/cilium/cilium/pkg/annotation"
	"github.com/cilium/cilium/pkg/backoff"
	"github.com/cilium/cilium/pkg/controller"
	"github.com/cilium/cilium/pkg/datapath/linux/probes"
	datapathOption "github.com/cilium/cilium/pkg/datapath/option"
	datapathTables "github.com/cilium/cilium/pkg/datapath/tables"
	datapath "github.com/cilium/cilium/pkg/datapath/types"
	"github.com/cilium/cilium/pkg/identity"
	k8smetrics "github.com/cilium/cilium/pkg/k8s/metrics"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	ipcachemap "github.com/cilium/cilium/pkg/maps/ipcache"
	ipmasqmap "github.com/cilium/cilium/pkg/maps/ipmasq"
	"github.com/cilium/cilium/pkg/maps/lbmap"
	"github.com/cilium/cilium/pkg/maps/lxcmap"
	"github.com/cilium/cilium/pkg/maps/metricsmap"
	"github.com/cilium/cilium/pkg/maps/ratelimitmap"
	"github.com/cilium/cilium/pkg/maps/timestamp"
	tunnelmap "github.com/cilium/cilium/pkg/maps/tunnel"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/status"
	"github.com/cilium/cilium/pkg/time"
	"github.com/cilium/cilium/pkg/version"
)

const (
	// k8sVersionCheckInterval is the interval in which the Kubernetes
	// version is verified even if connectivity is given
	k8sVersionCheckInterval = 15 * time.Minute

	// k8sMinimumEventHeartbeat is the time interval in which any received
	// event will be considered proof that the apiserver connectivity is
	// healthy
	k8sMinimumEventHeartbeat = time.Minute
)

type k8sVersion struct {
	version          string
	lastVersionCheck time.Time
	lock             lock.Mutex
}

func (k *k8sVersion) cachedVersion() (string, bool) {
	k.lock.Lock()
	defer k.lock.Unlock()

	if time.Since(k8smetrics.LastSuccessInteraction.Time()) > k8sMinimumEventHeartbeat {
		return "", false
	}

	if k.version == "" || time.Since(k.lastVersionCheck) > k8sVersionCheckInterval {
		return "", false
	}

	return k.version, true
}

func (k *k8sVersion) update(version *versionapi.Info) string {
	k.lock.Lock()
	defer k.lock.Unlock()

	k.version = fmt.Sprintf("%s.%s (%s) [%s]", version.Major, version.Minor, version.GitVersion, version.Platform)
	k.lastVersionCheck = time.Now()
	return k.version
}

var k8sVersionCache k8sVersion

func (d *Daemon) getK8sStatus() *models.K8sStatus {
	if !d.clientset.IsEnabled() {
		return &models.K8sStatus{State: models.StatusStateDisabled}
	}

	version, valid := k8sVersionCache.cachedVersion()
	if !valid {
		k8sVersion, err := d.clientset.Discovery().ServerVersion()
		if err != nil {
			return &models.K8sStatus{State: models.StatusStateFailure, Msg: err.Error()}
		}

		version = k8sVersionCache.update(k8sVersion)
	}

	k8sStatus := &models.K8sStatus{
		State:          models.StatusStateOk,
		Msg:            version,
		K8sAPIVersions: d.k8sWatcher.GetAPIGroups(),
	}

	return k8sStatus
}

func (d *Daemon) getMasqueradingStatus() *models.Masquerading {
	s := &models.Masquerading{
		Enabled: option.Config.MasqueradingEnabled(),
		EnabledProtocols: &models.MasqueradingEnabledProtocols{
			IPV4: option.Config.EnableIPv4Masquerade,
			IPV6: option.Config.EnableIPv6Masquerade,
		},
	}

	if !option.Config.MasqueradingEnabled() {
		return s
	}

	localNode, err := d.nodeLocalStore.Get(context.TODO())
	if err != nil {
		return s
	}

	if option.Config.EnableIPv4 {
		// SnatExclusionCidr is the legacy field, continue to provide
		// it for the time being
		s.SnatExclusionCidr = datapath.RemoteSNATDstAddrExclusionCIDRv4(localNode).String()
		s.SnatExclusionCidrV4 = datapath.RemoteSNATDstAddrExclusionCIDRv4(localNode).String()
	}

	if option.Config.EnableIPv6 {
		s.SnatExclusionCidrV6 = datapath.RemoteSNATDstAddrExclusionCIDRv6(localNode).String()
	}

	if option.Config.EnableBPFMasquerade {
		s.Mode = models.MasqueradingModeBPF
		s.IPMasqAgent = option.Config.EnableIPMasqAgent
		return s
	}

	s.Mode = models.MasqueradingModeIptables
	return s
}

func (d *Daemon) getSRv6Status() *models.Srv6 {
	return &models.Srv6{
		Enabled:       option.Config.EnableSRv6,
		Srv6EncapMode: option.Config.SRv6EncapMode,
	}
}

func (d *Daemon) getIPV6BigTCPStatus() *models.IPV6BigTCP {
	s := &models.IPV6BigTCP{
		Enabled: d.bigTCPConfig.EnableIPv6BIGTCP,
		MaxGRO:  int64(d.bigTCPConfig.GetGROIPv6MaxSize()),
		MaxGSO:  int64(d.bigTCPConfig.GetGSOIPv6MaxSize()),
	}

	return s
}

func (d *Daemon) getIPV4BigTCPStatus() *models.IPV4BigTCP {
	s := &models.IPV4BigTCP{
		Enabled: d.bigTCPConfig.EnableIPv4BIGTCP,
		MaxGRO:  int64(d.bigTCPConfig.GetGROIPv4MaxSize()),
		MaxGSO:  int64(d.bigTCPConfig.GetGSOIPv4MaxSize()),
	}

	return s
}

func (d *Daemon) getBandwidthManagerStatus() *models.BandwidthManager {
	s := &models.BandwidthManager{
		Enabled: d.bwManager.Enabled(),
	}

	if !d.bwManager.Enabled() {
		return s
	}

	s.CongestionControl = models.BandwidthManagerCongestionControlCubic
	if d.bwManager.BBREnabled() {
		s.CongestionControl = models.BandwidthManagerCongestionControlBbr
	}

	devs, _ := datapathTables.SelectedDevices(d.devices, d.db.ReadTxn())
	s.Devices = datapathTables.DeviceNames(devs)
	return s
}

func (d *Daemon) getRoutingStatus() *models.Routing {
	s := &models.Routing{
		IntraHostRoutingMode: models.RoutingIntraHostRoutingModeBPF,
		InterHostRoutingMode: models.RoutingInterHostRoutingModeTunnel,
		TunnelProtocol:       d.tunnelConfig.EncapProtocol().String(),
	}
	if option.Config.EnableHostLegacyRouting {
		s.IntraHostRoutingMode = models.RoutingIntraHostRoutingModeLegacy
	}
	if option.Config.RoutingMode == option.RoutingModeNative {
		s.InterHostRoutingMode = models.RoutingInterHostRoutingModeNative
	}
	return s
}

func (d *Daemon) getHostFirewallStatus() *models.HostFirewall {
	mode := models.HostFirewallModeDisabled
	if option.Config.EnableHostFirewall {
		mode = models.HostFirewallModeEnabled
	}
	devs, _ := datapathTables.SelectedDevices(d.devices, d.db.ReadTxn())
	return &models.HostFirewall{
		Mode:    mode,
		Devices: datapathTables.DeviceNames(devs),
	}
}

func (d *Daemon) getClockSourceStatus() *models.ClockSource {
	return timestamp.GetClockSourceFromOptions()
}

func (d *Daemon) getAttachModeStatus() models.AttachMode {
	mode := models.AttachModeTc
	if option.Config.EnableTCX && probes.HaveTCX() == nil {
		mode = models.AttachModeTcx
	}
	return mode
}

func (d *Daemon) getDatapathModeStatus() models.DatapathMode {
	mode := models.DatapathModeVeth
	switch option.Config.DatapathMode {
	case datapathOption.DatapathModeNetkit:
		mode = models.DatapathModeNetkit
	case datapathOption.DatapathModeNetkitL2:
		mode = models.DatapathModeNetkitDashL2
	}
	return mode
}

func (d *Daemon) getCNIChainingStatus() *models.CNIChainingStatus {
	mode := d.cniConfigManager.GetChainingMode()
	if len(mode) == 0 {
		mode = models.CNIChainingStatusModeNone
	}
	return &models.CNIChainingStatus{
		Mode: mode,
	}
}

func (d *Daemon) getKubeProxyReplacementStatus() *models.KubeProxyReplacement {
	var mode string
	switch option.Config.KubeProxyReplacement {
	case option.KubeProxyReplacementTrue:
		mode = models.KubeProxyReplacementModeTrue
	case option.KubeProxyReplacementFalse:
		mode = models.KubeProxyReplacementModeFalse
	}

	devices, _ := datapathTables.SelectedDevices(d.devices, d.db.ReadTxn())
	devicesList := make([]*models.KubeProxyReplacementDeviceListItems0, len(devices))
	for i, dev := range devices {
		info := &models.KubeProxyReplacementDeviceListItems0{
			Name: dev.Name,
			IP:   make([]string, len(dev.Addrs)),
		}
		for _, addr := range dev.Addrs {
			info.IP = append(info.IP, addr.Addr.String())
		}
		devicesList[i] = info
	}

	features := &models.KubeProxyReplacementFeatures{
		NodePort:              &models.KubeProxyReplacementFeaturesNodePort{},
		HostPort:              &models.KubeProxyReplacementFeaturesHostPort{},
		ExternalIPs:           &models.KubeProxyReplacementFeaturesExternalIPs{},
		SocketLB:              &models.KubeProxyReplacementFeaturesSocketLB{},
		SocketLBTracing:       &models.KubeProxyReplacementFeaturesSocketLBTracing{},
		SessionAffinity:       &models.KubeProxyReplacementFeaturesSessionAffinity{},
		Nat46X64:              &models.KubeProxyReplacementFeaturesNat46X64{},
		BpfSocketLBHostnsOnly: option.Config.BPFSocketLBHostnsOnly,
	}
	if option.Config.EnableNodePort {
		features.NodePort.Enabled = true
		features.NodePort.Mode = strings.ToUpper(option.Config.NodePortMode)
		switch option.Config.LoadBalancerDSRDispatch {
		case option.DSRDispatchIPIP:
			features.NodePort.DsrMode = models.KubeProxyReplacementFeaturesNodePortDsrModeIPIP
		case option.DSRDispatchOption:
			features.NodePort.DsrMode = models.KubeProxyReplacementFeaturesNodePortDsrModeIPOptionExtension
		case option.DSRDispatchGeneve:
			features.NodePort.DsrMode = models.KubeProxyReplacementFeaturesNodePortDsrModeGeneve
		}
		if option.Config.NodePortMode == option.NodePortModeHybrid {
			//nolint:staticcheck
			features.NodePort.Mode = strings.Title(option.Config.NodePortMode)
		}
		features.NodePort.Algorithm = models.KubeProxyReplacementFeaturesNodePortAlgorithmRandom
		if option.Config.NodePortAlg == option.NodePortAlgMaglev {
			features.NodePort.Algorithm = models.KubeProxyReplacementFeaturesNodePortAlgorithmMaglev
			features.NodePort.LutSize = int64(d.maglevConfig.MaglevTableSize)
		}
		if option.Config.LoadBalancerAlgorithmAnnotation {
			features.NodePort.LutSize = int64(d.maglevConfig.MaglevTableSize)
		}
		if option.Config.NodePortAcceleration == option.NodePortAccelerationGeneric {
			features.NodePort.Acceleration = models.KubeProxyReplacementFeaturesNodePortAccelerationGeneric
		} else {
			features.NodePort.Acceleration = strings.Title(option.Config.NodePortAcceleration)
		}
		features.NodePort.PortMin = int64(option.Config.NodePortMin)
		features.NodePort.PortMax = int64(option.Config.NodePortMax)
	}
	if option.Config.EnableHostPort {
		features.HostPort.Enabled = true
	}
	if option.Config.EnableExternalIPs {
		features.ExternalIPs.Enabled = true
	}
	if option.Config.EnableSocketLB {
		features.SocketLB.Enabled = true
		features.SocketLBTracing.Enabled = true
	}
	if option.Config.EnableSessionAffinity {
		features.SessionAffinity.Enabled = true
	}
	if option.Config.NodePortNat46X64 || option.Config.EnableNat46X64Gateway {
		features.Nat46X64.Enabled = true
		gw := &models.KubeProxyReplacementFeaturesNat46X64Gateway{
			Enabled:  option.Config.EnableNat46X64Gateway,
			Prefixes: make([]string, 0),
		}
		if option.Config.EnableNat46X64Gateway {
			gw.Prefixes = append(gw.Prefixes, option.Config.IPv6NAT46x64CIDR)
		}
		features.Nat46X64.Gateway = gw

		svc := &models.KubeProxyReplacementFeaturesNat46X64Service{
			Enabled: option.Config.NodePortNat46X64,
		}
		features.Nat46X64.Service = svc
	}
	if option.Config.EnableNodePort {
		if option.Config.LoadBalancerAlgorithmAnnotation {
			features.Annotations = append(features.Annotations, annotation.ServiceLoadBalancingAlgorithm)
		}
		if option.Config.LoadBalancerModeAnnotation {
			features.Annotations = append(features.Annotations, annotation.ServiceForwardingMode)
		}
		features.Annotations = append(features.Annotations, annotation.ServiceNodeExposure)
		features.Annotations = append(features.Annotations, annotation.ServiceNodeSelectorExposure)
		features.Annotations = append(features.Annotations, annotation.ServiceTypeExposure)
		features.Annotations = append(features.Annotations, annotation.ServiceProxyDelegation)
		if option.Config.EnableSVCSourceRangeCheck {
			features.Annotations = append(features.Annotations, annotation.ServiceSourceRangesPolicy)
		}
		sort.Strings(features.Annotations)
	}

	var directRoutingDevice string
	drd, _ := d.directRoutingDev.Get(context.TODO(), d.db.ReadTxn())
	if drd != nil {
		directRoutingDevice = drd.Name
	}

	return &models.KubeProxyReplacement{
		Mode:                mode,
		Devices:             datapathTables.DeviceNames(devices),
		DeviceList:          devicesList,
		DirectRoutingDevice: directRoutingDevice,
		Features:            features,
	}
}

func (d *Daemon) getBPFMapStatus() *models.BPFMapStatus {
	return &models.BPFMapStatus{
		DynamicSizeRatio: option.Config.BPFMapsDynamicSizeRatio,
		Maps: []*models.BPFMapProperties{
			{
				Name: "Auth",
				Size: int64(option.Config.AuthMapEntries),
			},
			{
				Name: "Non-TCP connection tracking",
				Size: int64(option.Config.CTMapEntriesGlobalAny),
			},
			{
				Name: "TCP connection tracking",
				Size: int64(option.Config.CTMapEntriesGlobalTCP),
			},
			{
				Name: "Endpoints",
				Size: int64(lxcmap.MaxEntries),
			},
			{
				Name: "IP cache",
				Size: int64(ipcachemap.MaxEntries),
			},
			{
				Name: "IPv4 masquerading agent",
				Size: int64(ipmasqmap.MaxEntriesIPv4),
			},
			{
				Name: "IPv6 masquerading agent",
				Size: int64(ipmasqmap.MaxEntriesIPv6),
			},
			{
				Name: "IPv4 fragmentation",
				Size: int64(option.Config.FragmentsMapEntries),
			},
			{
				Name: "IPv4 service", // cilium_lb4_services_v2
				Size: int64(lbmap.ServiceMapMaxEntries),
			},
			{
				Name: "IPv6 service", // cilium_lb6_services_v2
				Size: int64(lbmap.ServiceMapMaxEntries),
			},
			{
				Name: "IPv4 service backend", // cilium_lb4_backends_v2
				Size: int64(lbmap.ServiceBackEndMapMaxEntries),
			},
			{
				Name: "IPv6 service backend", // cilium_lb6_backends_v2
				Size: int64(lbmap.ServiceBackEndMapMaxEntries),
			},
			{
				Name: "IPv4 service reverse NAT", // cilium_lb4_reverse_nat
				Size: int64(lbmap.RevNatMapMaxEntries),
			},
			{
				Name: "IPv6 service reverse NAT", // cilium_lb6_reverse_nat
				Size: int64(lbmap.RevNatMapMaxEntries),
			},
			{
				Name: "Metrics",
				Size: int64(metricsmap.MaxEntries),
			},
			{
				Name: "Ratelimit metrics",
				Size: int64(ratelimitmap.MaxMetricsEntries),
			},
			{
				Name: "NAT",
				Size: int64(option.Config.NATMapEntriesGlobal),
			},
			{
				Name: "Neighbor table",
				Size: int64(option.Config.NeighMapEntriesGlobal),
			},
			{
				Name: "Endpoint policy",
				Size: int64(d.policyMapFactory.PolicyMaxEntries()),
			},
			{
				Name: "Policy stats",
				Size: int64(d.policyMapFactory.StatsMaxEntries()),
			},
			{
				Name: "Session affinity",
				Size: int64(lbmap.AffinityMapMaxEntries),
			},
			{
				Name: "Sock reverse NAT",
				Size: int64(option.Config.SockRevNatEntries),
			},
			{
				Name: "Tunnel",
				Size: int64(tunnelmap.MaxEntries),
			},
		},
	}
}

func getHealthzHandler(d *Daemon, params GetHealthzParams) middleware.Responder {
	brief := params.Brief != nil && *params.Brief
	requireK8sConnectivity := params.RequireK8sConnectivity != nil && *params.RequireK8sConnectivity
	sr := d.getStatus(brief, requireK8sConnectivity)
	return NewGetHealthzOK().WithPayload(&sr)
}

// getStatus returns the daemon status. If brief is provided a minimal version
// of the StatusResponse is provided.
func (d *Daemon) getStatus(brief bool, requireK8sConnectivity bool) models.StatusResponse {
	staleProbes := d.statusCollector.GetStaleProbes()
	stale := make(map[string]strfmt.DateTime, len(staleProbes))
	for probe, startTime := range staleProbes {
		stale[probe] = strfmt.DateTime(startTime)
	}

	d.statusCollectMutex.RLock()
	defer d.statusCollectMutex.RUnlock()

	var sr models.StatusResponse
	if brief {
		csCopy := new(models.ClusterStatus)
		if d.statusResponse.Cluster != nil && d.statusResponse.Cluster.CiliumHealth != nil {
			in, out := &d.statusResponse.Cluster.CiliumHealth, &csCopy.CiliumHealth
			*out = new(models.Status)
			**out = **in
		}
		var minimalControllers models.ControllerStatuses
		if d.statusResponse.Controllers != nil {
			for _, c := range d.statusResponse.Controllers {
				if c.Status == nil {
					continue
				}
				// With brief, the client should only care if a single controller
				// is failing and its status so we don't need to continuing
				// checking for failure messages for the remaining controllers.
				if c.Status.LastFailureMsg != "" {
					minimalControllers = append(minimalControllers, c.DeepCopy())
					break
				}
			}
		}
		sr = models.StatusResponse{
			Cluster:     csCopy,
			Controllers: minimalControllers,
		}
	} else {
		// d.statusResponse contains references, so we do a deep copy to be able to
		// safely use sr after the method has returned
		sr = *d.statusResponse.DeepCopy()
	}

	sr.Stale = stale

	// CiliumVersion definition
	ver := version.GetCiliumVersion()
	ciliumVer := fmt.Sprintf("%s (v%s-%s)", ver.Version, ver.Version, ver.Revision)

	switch {
	case len(sr.Stale) > 0:
		msg := "Stale status data"
		sr.Cilium = &models.Status{
			State: models.StatusStateWarning,
			Msg:   fmt.Sprintf("%s    %s", ciliumVer, msg),
		}
	case d.statusResponse.Kvstore != nil &&
		d.statusResponse.Kvstore.State != models.StatusStateOk &&
		d.statusResponse.Kvstore.State != models.StatusStateDisabled:
		msg := "Kvstore service is not ready: " + d.statusResponse.Kvstore.Msg
		sr.Cilium = &models.Status{
			State: d.statusResponse.Kvstore.State,
			Msg:   fmt.Sprintf("%s    %s", ciliumVer, msg),
		}
	case d.statusResponse.ContainerRuntime != nil && d.statusResponse.ContainerRuntime.State != models.StatusStateOk:
		msg := "Container runtime is not ready: " + d.statusResponse.ContainerRuntime.Msg
		if d.statusResponse.ContainerRuntime.State == models.StatusStateDisabled {
			msg = "Container runtime is disabled"
		}
		sr.Cilium = &models.Status{
			State: d.statusResponse.ContainerRuntime.State,
			Msg:   fmt.Sprintf("%s    %s", ciliumVer, msg),
		}
	case d.clientset.IsEnabled() && d.statusResponse.Kubernetes != nil && d.statusResponse.Kubernetes.State != models.StatusStateOk && requireK8sConnectivity:
		msg := "Kubernetes service is not ready: " + d.statusResponse.Kubernetes.Msg
		sr.Cilium = &models.Status{
			State: d.statusResponse.Kubernetes.State,
			Msg:   fmt.Sprintf("%s    %s", ciliumVer, msg),
		}
	case d.statusResponse.CniFile != nil && d.statusResponse.CniFile.State == models.StatusStateFailure:
		msg := "Could not write CNI config file: " + d.statusResponse.CniFile.Msg
		sr.Cilium = &models.Status{
			State: models.StatusStateFailure,
			Msg:   fmt.Sprintf("%s    %s", ciliumVer, msg),
		}
	default:
		sr.Cilium = &models.Status{
			State: models.StatusStateOk,
			Msg:   ciliumVer,
		}
	}

	return sr
}

func (d *Daemon) getIdentityRange() *models.IdentityRange {
	s := &models.IdentityRange{
		MinIdentity: int64(identity.GetMinimalAllocationIdentity(d.clusterInfo.ID)),
		MaxIdentity: int64(identity.GetMaximumAllocationIdentity(d.clusterInfo.ID)),
	}

	return s
}

func (d *Daemon) startStatusCollector(ctx context.Context, cleaner *daemonCleanup) error {
	probes := []status.Probe{
		{
			Name: "kvstore",
			Probe: func(ctx context.Context) (interface{}, error) {
				if option.Config.KVStore == "" {
					return &models.Status{State: models.StatusStateDisabled}, nil
				} else {
					return kvstore.Client().Status(), nil
				}
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				if status.Err != nil {
					d.statusResponse.Kvstore = &models.Status{
						State: models.StatusStateFailure,
						Msg:   status.Err.Error(),
					}
					return
				}

				if kvstore, ok := status.Data.(*models.Status); ok {
					d.statusResponse.Kvstore = kvstore
				}
			},
		},
		{
			Name: "kubernetes",
			Interval: func(failures int) time.Duration {
				if failures > 0 {
					// While failing, we want an initial
					// quick retry with exponential backoff
					// to avoid continuous load on the
					// apiserver
					return backoff.CalculateDuration(5*time.Second, 2*time.Minute, 2.0, false, failures)
				}

				// The base interval is dependant on the
				// cluster size. One status interval does not
				// automatically translate to an apiserver
				// interaction as any regular apiserver
				// interaction is also used as an indication of
				// successful connectivity so we can continue
				// to be fairly aggressive.
				//
				// 1     |    7s
				// 2     |   12s
				// 4     |   15s
				// 64    |   42s
				// 512   | 1m02s
				// 2048  | 1m15s
				// 8192  | 1m30s
				// 16384 | 1m32s
				return d.nodeDiscovery.Manager.ClusterSizeDependantInterval(10 * time.Second)
			},
			Probe: func(ctx context.Context) (interface{}, error) {
				return d.getK8sStatus(), nil
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				if status.Err != nil {
					d.statusResponse.Kubernetes = &models.K8sStatus{
						State: models.StatusStateFailure,
						Msg:   status.Err.Error(),
					}
					return
				}
				if s, ok := status.Data.(*models.K8sStatus); ok {
					d.statusResponse.Kubernetes = s
				}
			},
		},
		{
			Name: "ipam",
			Probe: func(ctx context.Context) (interface{}, error) {
				return d.DumpIPAM(), nil
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				// IPAMStatus has no way to show errors
				if status.Err == nil {
					if s, ok := status.Data.(*models.IPAMStatus); ok {
						d.statusResponse.Ipam = s
					}
				}
			},
		},
		{
			Name: "node-monitor",
			Probe: func(ctx context.Context) (interface{}, error) {
				return d.monitorAgent.State(), nil
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				// NodeMonitor has no way to show errors
				if status.Err == nil {
					if s, ok := status.Data.(*models.MonitorStatus); ok {
						d.statusResponse.NodeMonitor = s
					}
				}
			},
		},
		{
			Name: "cluster",
			Probe: func(ctx context.Context) (interface{}, error) {
				clusterStatus := &models.ClusterStatus{
					Self: nodeTypes.GetAbsoluteNodeName(),
				}
				return clusterStatus, nil
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				// ClusterStatus has no way to report errors
				if status.Err == nil {
					if s, ok := status.Data.(*models.ClusterStatus); ok {
						if d.statusResponse.Cluster != nil {
							// NB: CiliumHealth is set concurrently by the
							// "cilium-health" probe, so do not override it
							s.CiliumHealth = d.statusResponse.Cluster.CiliumHealth
						}
						d.statusResponse.Cluster = s
					}
				}
			},
		},
		{
			Name: "cilium-health",
			Probe: func(ctx context.Context) (interface{}, error) {
				if d.ciliumHealth == nil {
					return nil, nil
				}
				return d.ciliumHealth.GetStatus(), nil
			},
			OnStatusUpdate: func(status status.Status) {
				if d.ciliumHealth == nil {
					return
				}

				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				if d.statusResponse.Cluster == nil {
					d.statusResponse.Cluster = &models.ClusterStatus{}
				}
				if status.Err != nil {
					d.statusResponse.Cluster.CiliumHealth = &models.Status{
						State: models.StatusStateFailure,
						Msg:   status.Err.Error(),
					}
					return
				}
				if s, ok := status.Data.(*models.Status); ok {
					d.statusResponse.Cluster.CiliumHealth = s
				}
			},
		},
		{
			Name: "l7-proxy",
			Probe: func(ctx context.Context) (interface{}, error) {
				if d.l7Proxy == nil {
					return nil, nil
				}
				return d.l7Proxy.GetStatusModel(), nil
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				// ProxyStatus has no way to report errors
				if status.Err == nil {
					if s, ok := status.Data.(*models.ProxyStatus); ok {
						d.statusResponse.Proxy = s
					}
				}
			},
		},
		{
			Name: "controllers",
			Probe: func(ctx context.Context) (interface{}, error) {
				return controller.GetGlobalStatus(), nil
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				// ControllerStatuses has no way to report errors
				if status.Err == nil {
					if s, ok := status.Data.(models.ControllerStatuses); ok {
						d.statusResponse.Controllers = s
					}
				}
			},
		},
		{
			Name: "clustermesh",
			Probe: func(ctx context.Context) (interface{}, error) {
				if d.clustermesh == nil {
					return nil, nil
				}
				return d.clustermesh.Status(), nil
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				if status.Err == nil {
					if s, ok := status.Data.(*models.ClusterMeshStatus); ok {
						d.statusResponse.ClusterMesh = s
					}
				}
			},
		},
		{
			Name: "hubble",
			Probe: func(ctx context.Context) (interface{}, error) {
				return d.hubble.Status(ctx), nil
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				if status.Err == nil {
					if s, ok := status.Data.(*models.HubbleStatus); ok {
						d.statusResponse.Hubble = s
					}
				}
			},
		},
		{
			Name: "encryption",
			Probe: func(ctx context.Context) (interface{}, error) {
				switch {
				case option.Config.EnableIPSec:
					return &models.EncryptionStatus{
						Mode: models.EncryptionStatusModeIPsec,
					}, nil
				case option.Config.EnableWireguard:
					var msg string
					status, err := d.wireguardAgent.Status(false)
					if err != nil {
						msg = err.Error()
					}
					return &models.EncryptionStatus{
						Mode:      models.EncryptionStatusModeWireguard,
						Msg:       msg,
						Wireguard: status,
					}, nil
				default:
					return &models.EncryptionStatus{
						Mode: models.EncryptionStatusModeDisabled,
					}, nil
				}
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				if status.Err == nil {
					if s, ok := status.Data.(*models.EncryptionStatus); ok {
						d.statusResponse.Encryption = s
					}
				}
			},
		},
		{
			Name: "kube-proxy-replacement",
			Probe: func(ctx context.Context) (interface{}, error) {
				return d.getKubeProxyReplacementStatus(), nil
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				if status.Err == nil {
					if s, ok := status.Data.(*models.KubeProxyReplacement); ok {
						d.statusResponse.KubeProxyReplacement = s
					}
				}
			},
		},
		{
			Name: "auth-cert-provider",
			Probe: func(ctx context.Context) (interface{}, error) {
				if d.authManager == nil {
					return &models.Status{State: models.StatusStateDisabled}, nil
				}

				return d.authManager.CertProviderStatus(), nil
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				if status.Err == nil {
					if s, ok := status.Data.(*models.Status); ok {
						d.statusResponse.AuthCertificateProvider = s
					}
				}
			},
		},
		{
			Name: "cni-config",
			Probe: func(ctx context.Context) (interface{}, error) {
				if d.cniConfigManager == nil {
					return nil, nil
				}
				return d.cniConfigManager.Status(), nil
			},
			OnStatusUpdate: func(status status.Status) {
				d.statusCollectMutex.Lock()
				defer d.statusCollectMutex.Unlock()

				if status.Err == nil {
					if s, ok := status.Data.(*models.Status); ok {
						d.statusResponse.CniFile = s
					}
				}
			},
		},
	}

	d.statusResponse.Masquerading = d.getMasqueradingStatus()
	d.statusResponse.IPV6BigTCP = d.getIPV6BigTCPStatus()
	d.statusResponse.IPV4BigTCP = d.getIPV4BigTCPStatus()
	d.statusResponse.BandwidthManager = d.getBandwidthManagerStatus()
	d.statusResponse.HostFirewall = d.getHostFirewallStatus()
	d.statusResponse.Routing = d.getRoutingStatus()
	d.statusResponse.ClockSource = d.getClockSourceStatus()
	d.statusResponse.BpfMaps = d.getBPFMapStatus()
	d.statusResponse.CniChaining = d.getCNIChainingStatus()
	d.statusResponse.IdentityRange = d.getIdentityRange()
	d.statusResponse.Srv6 = d.getSRv6Status()
	d.statusResponse.AttachMode = d.getAttachModeStatus()
	d.statusResponse.DatapathMode = d.getDatapathModeStatus()

	d.statusCollector = status.NewCollector(probes, status.DefaultConfig)

	// Block until all probes have been executed at least once, to make sure that
	// the status has been fully initialized once we exit from this function.
	if err := d.statusCollector.WaitForFirstRun(ctx); err != nil {
		return fmt.Errorf("waiting for first run: %w", err)
	}

	// Set up a signal handler function which prints out logs related to daemon status.
	cleaner.cleanupFuncs.Add(func() {
		// If the KVstore state is not OK, print help for user.
		if d.statusResponse.Kvstore != nil &&
			d.statusResponse.Kvstore.State != models.StatusStateOk &&
			d.statusResponse.Kvstore.State != models.StatusStateDisabled {
			helpMsg := "cilium-agent depends on the availability of cilium-operator/etcd-cluster. " +
				"Check if the cilium-operator pod and etcd-cluster are running and do not have any " +
				"warnings or error messages."
			log.WithFields(logrus.Fields{
				"status":              d.statusResponse.Kvstore.Msg,
				logfields.HelpMessage: helpMsg,
			}).Error("KVStore state not OK")

		}

		d.statusCollector.Close()
	})

	return nil
}
