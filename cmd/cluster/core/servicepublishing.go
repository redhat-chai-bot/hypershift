package core

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/support/netutil"
	"github.com/openshift/hypershift/support/publishing"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/spf13/pflag"
)

func bindPublishingOptions(opts *RawCreateOptions, flags *pflag.FlagSet) {
	flags.StringVar(&opts.PublishingType, "publishing-type", opts.PublishingType, "Opt in to spec.publishing: SingleIngressPoint, DedicatedAPIServerEndpoint, or NodePort. If omitted, retain legacy spec.services output. Requires management-cluster ServicePublishingAPI feature gate.")
	flags.StringVar(&opts.PublishingExposure, "publishing-exposure", opts.PublishingExposure, "SingleIngressPoint exposure: LoadBalancer (default), NodePort, or PlatformProvided (default for managed Azure).")
	flags.StringVar(&opts.PublishingAddress, "publishing-address", opts.PublishingAddress, "Reachable node address for NodePort publishing or SingleIngressPoint with NodePort exposure; defaults to the platform's API server address when available.")
	flags.Int32Var(&opts.PublishingPort, "publishing-port", opts.PublishingPort, "Router NodePort for SingleIngressPoint with NodePort exposure (30000-32767); omitted ports are dynamically assigned.")
	flags.StringVar(&opts.PublishingHostname, "publishing-hostname", opts.PublishingHostname, "Optional router LoadBalancer hostname for SingleIngressPoint with LoadBalancer exposure.")
	flags.StringArrayVar(&opts.PublishingServiceHostnames, "publishing-service-hostname", opts.PublishingServiceHostnames, "Service hostname override (APIServer=api.example.com); repeat for multiple services. DedicatedAPIServerEndpoint supports only APIServer and OAuthServer.")
	flags.StringArrayVar(&opts.PublishingServicePorts, "publishing-service-port", opts.PublishingServicePorts, "Per-service NodePort override (APIServer=30443); repeat for multiple services. Only valid with --publishing-type=NodePort.")
}

func (opts *RawCreateOptions) validatePublishingOptions() error {
	if opts.PublishingType == "" {
		if opts.PublishingExposure != "" || opts.PublishingAddress != "" || opts.PublishingPort != 0 || opts.PublishingHostname != "" || len(opts.PublishingServiceHostnames) != 0 || len(opts.PublishingServicePorts) != 0 {
			return fmt.Errorf("publishing options require --publishing-type")
		}
		return nil
	}
	if err := opts.validatePublishingFlagCombination(); err != nil {
		return err
	}
	if err := opts.validatePublishingValues(); err != nil {
		return err
	}
	hostnames, err := parseKeyValuePairs(opts.PublishingServiceHostnames, "publishing service hostname")
	if err != nil {
		return err
	}
	if err := opts.validatePublishingServiceHostnames(hostnames); err != nil {
		return err
	}
	ports, err := parseKeyValuePairs(opts.PublishingServicePorts, "publishing service port")
	if err != nil {
		return err
	}
	return validatePublishingServicePorts(ports)
}

// validatePublishingFlagCombination rejects flags that do not belong to the selected publishing type.
func (opts *RawCreateOptions) validatePublishingFlagCombination() error {
	switch opts.PublishingType {
	case "SingleIngressPoint":
		switch opts.PublishingExposure {
		case "", "LoadBalancer", "NodePort", "PlatformProvided":
		default:
			return fmt.Errorf("unsupported --publishing-exposure %q: supported values are LoadBalancer, NodePort, PlatformProvided", opts.PublishingExposure)
		}
		if opts.PublishingHostname != "" && opts.PublishingExposure != "" && opts.PublishingExposure != "LoadBalancer" {
			return fmt.Errorf("--publishing-hostname requires LoadBalancer exposure")
		}
		if (opts.PublishingAddress != "" || opts.PublishingPort != 0) && opts.PublishingExposure != "NodePort" {
			return fmt.Errorf("--publishing-address and --publishing-port require NodePort exposure with SingleIngressPoint")
		}
		if len(opts.PublishingServicePorts) != 0 {
			return fmt.Errorf("--publishing-service-port requires --publishing-type=NodePort")
		}
	case "DedicatedAPIServerEndpoint", "NodePort":
		if opts.PublishingExposure != "" || opts.PublishingHostname != "" || opts.PublishingPort != 0 {
			return fmt.Errorf("--publishing-exposure, --publishing-hostname and --publishing-port require --publishing-type=SingleIngressPoint")
		}
		if opts.PublishingType == "DedicatedAPIServerEndpoint" && (opts.PublishingAddress != "" || len(opts.PublishingServicePorts) != 0) {
			return fmt.Errorf("--publishing-address and --publishing-service-port require NodePort publishing")
		}
		if opts.PublishingType == "NodePort" && len(opts.PublishingServiceHostnames) != 0 {
			return fmt.Errorf("--publishing-service-hostname is not supported with NodePort publishing")
		}
	default:
		return fmt.Errorf("unsupported --publishing-type %q: supported values are SingleIngressPoint, DedicatedAPIServerEndpoint, NodePort", opts.PublishingType)
	}
	return nil
}

// validatePublishingValues checks the scalar publishing flags in isolation.
func (opts *RawCreateOptions) validatePublishingValues() error {
	if opts.PublishingPort != 0 && (opts.PublishingPort < 30000 || opts.PublishingPort > 32767) {
		return fmt.Errorf("--publishing-port must be between 30000 and 32767")
	}
	if opts.PublishingAddress != "" && !validPublishingAddress(opts.PublishingAddress) {
		return fmt.Errorf("--publishing-address must be a hostname, IPv4 address, or IPv6 address")
	}
	if opts.PublishingHostname != "" && !validPublishingHostname(opts.PublishingHostname) {
		return fmt.Errorf("--publishing-hostname must be a valid DNS hostname")
	}
	return nil
}

// validatePublishingServiceHostnames checks each --publishing-service-hostname override.
func (opts *RawCreateOptions) validatePublishingServiceHostnames(hostnames map[string]string) error {
	for service, hostname := range hostnames {
		switch service {
		case "APIServer", "OAuthServer":
		case "Konnectivity", "Ignition":
			if opts.PublishingType == "DedicatedAPIServerEndpoint" {
				return fmt.Errorf("%s hostname cannot be overridden with DedicatedAPIServerEndpoint", service)
			}
		default:
			return fmt.Errorf("unsupported publishing service %q", service)
		}
		if !validPublishingHostname(hostname) {
			return fmt.Errorf("invalid hostname %q for %s", hostname, service)
		}
	}
	return nil
}

// validatePublishingServicePorts checks each --publishing-service-port override.
func validatePublishingServicePorts(ports map[string]string) error {
	usedPorts := map[int64]string{}
	for service, portValue := range ports {
		switch service {
		case "APIServer", "OAuthServer", "Konnectivity", "Ignition":
		default:
			return fmt.Errorf("unsupported publishing service %q", service)
		}
		port, err := strconv.ParseInt(portValue, 10, 32)
		if err != nil || port < 30000 || port > 32767 {
			return fmt.Errorf("publishing port for %s must be between 30000 and 32767", service)
		}
		if other, exists := usedPorts[port]; exists {
			return fmt.Errorf("publishing port %d is shared by %s and %s", port, service, other)
		}
		usedPorts[port] = service
	}
	return nil
}

func validPublishingHostname(hostname string) bool {
	if len(hostname) == 0 || len(hostname) > 253 {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(validation.IsDNS1123Label(strings.ToLower(label))) != 0 {
			return false
		}
	}
	return true
}

func validPublishingAddress(address string) bool {
	if net.ParseIP(address) != nil {
		return true
	}
	labels := strings.Split(address, ".")
	if len(labels) == 4 {
		allNumeric := true
		for _, label := range labels {
			if label == "" || strings.IndexFunc(label, func(character rune) bool { return character < '0' || character > '9' }) >= 0 {
				allNumeric = false
			}
		}
		if allNumeric {
			return false
		}
	}
	return validPublishingHostname(address)
}

func applyPublishing(cluster *hyperv1.HostedCluster, opts *RawCreateOptions) error {
	if err := opts.validatePublishingOptions(); err != nil {
		return err
	}
	if opts.PublishingType == "" {
		return nil
	}
	hostnames, err := parseKeyValuePairs(opts.PublishingServiceHostnames, "publishing service hostname")
	if err != nil {
		return err
	}
	ports, err := parseKeyValuePairs(opts.PublishingServicePorts, "publishing service port")
	if err != nil {
		return err
	}
	address, oauthLoadBalancer := inheritPublishingDefaults(cluster, opts, hostnames)
	policy := hyperv1.ServicePublishing{Type: hyperv1.ServicePublishingType(opts.PublishingType)}
	switch opts.PublishingType {
	case "SingleIngressPoint":
		ingress, err := singleIngressPointPublishing(cluster, opts, hostnames, address, oauthLoadBalancer)
		if err != nil {
			return err
		}
		policy.SingleIngressPoint = ingress
	case "DedicatedAPIServerEndpoint":
		policy.DedicatedAPIServerEndpoint = dedicatedAPIServerEndpointPublishing(hostnames, oauthLoadBalancer)
	case "NodePort":
		nodePort, err := nodePortPublishing(address, ports)
		if err != nil {
			return err
		}
		policy.NodePort = nodePort
	}
	if _, err := publishing.ToServices(&policy, cluster.Spec.Platform); err != nil {
		return err
	}
	cluster.Spec.Publishing = policy
	cluster.Spec.Services = nil
	return nil
}

// publishingServiceNames lists the services the publishing presets can carry, in reporting order.
var publishingServiceNames = []string{"APIServer", "OAuthServer", "Konnectivity", "Ignition"}

// inheritPublishingDefaults fills hostnames from the legacy services the cluster already carries and
// reports the NodePort address and whether OAuth already had its own LoadBalancer.
func inheritPublishingDefaults(cluster *hyperv1.HostedCluster, opts *RawCreateOptions, hostnames map[string]string) (string, bool) {
	address := opts.PublishingAddress
	oauthLoadBalancer := false
	for _, service := range cluster.Spec.Services {
		serviceName := string(service.Service)
		if _, overridden := hostnames[serviceName]; !overridden {
			if service.Route != nil && service.Type == hyperv1.Route && service.Route.Hostname != "" {
				hostnames[serviceName] = service.Route.Hostname
			} else if service.LoadBalancer != nil && service.Type == hyperv1.LoadBalancer && service.LoadBalancer.Hostname != "" {
				hostnames[serviceName] = service.LoadBalancer.Hostname
			}
		}
		if address == "" && service.NodePort != nil {
			address = service.NodePort.Address
		}
		if service.Service == hyperv1.OAuthServer && service.Type == hyperv1.LoadBalancer {
			oauthLoadBalancer = true
		}
	}
	if opts.ExternalDNSDomain != "" && opts.PublishingType != "NodePort" {
		for serviceName, prefix := range map[string]string{"APIServer": "api", "OAuthServer": "oauth"} {
			if hostnames[serviceName] == "" {
				hostnames[serviceName] = fmt.Sprintf("%s-%s.%s", prefix, cluster.Name, opts.ExternalDNSDomain)
			}
		}
	}
	return address, oauthLoadBalancer
}

// singleIngressPointPublishing builds the SingleIngressPoint preset for the selected exposure.
func singleIngressPointPublishing(cluster *hyperv1.HostedCluster, opts *RawCreateOptions, hostnames map[string]string, address string, oauthLoadBalancer bool) (hyperv1.SingleIngressPointPublishing, error) {
	exposure := opts.PublishingExposure
	if exposure == "" {
		exposure = "LoadBalancer"
		if cluster.Spec.Platform.Type == hyperv1.IBMCloudPlatform || netutil.IsAroHCPByHC(cluster) {
			exposure = "PlatformProvided"
		}
	}
	if hostnames["APIServer"] == "" {
		return hyperv1.SingleIngressPointPublishing{}, fmt.Errorf("SingleIngressPoint requires an APIServer hostname: set --external-dns-domain or --publishing-service-hostname=APIServer=<hostname>")
	}
	ingress := hyperv1.SingleIngressPointPublishing{Exposure: hyperv1.IngressExposureType(exposure)}
	switch exposure {
	case "LoadBalancer":
		ingress.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{Hostname: opts.PublishingHostname}
	case "NodePort":
		if cluster.Spec.Platform.Type != hyperv1.AgentPlatform && cluster.Spec.Platform.Type != hyperv1.KubevirtPlatform && cluster.Spec.Platform.Type != hyperv1.NonePlatform {
			return hyperv1.SingleIngressPointPublishing{}, fmt.Errorf("SingleIngressPoint with NodePort exposure is only supported on Agent, KubeVirt, and None")
		}
		if address == "" {
			return hyperv1.SingleIngressPointPublishing{}, fmt.Errorf("SingleIngressPoint with NodePort exposure requires --publishing-address or a platform API server address")
		}
		ingress.NodePort = hyperv1.IngressNodePortConfig{Address: address}
		if opts.PublishingPort != 0 {
			ingress.NodePort.Port = opts.PublishingPort
		}
	case "PlatformProvided":
		if opts.PublishingHostname != "" {
			return hyperv1.SingleIngressPointPublishing{}, fmt.Errorf("--publishing-hostname requires LoadBalancer exposure")
		}
	}
	for _, serviceName := range publishingServiceNames {
		service := hyperv1.SingleIngressPointService{Name: hyperv1.IngressServiceName(serviceName), Hostname: hostnames[serviceName]}
		if serviceName == "OAuthServer" && oauthLoadBalancer {
			service.Exposure = hyperv1.ServiceExposureDedicatedLoadBalancer
		}
		if service.Hostname != "" || service.Exposure != "" {
			ingress.Services = append(ingress.Services, service)
		}
	}
	return ingress, nil
}

// dedicatedAPIServerEndpointPublishing builds the DedicatedAPIServerEndpoint preset, or nil when no
// endpoint needs an override.
func dedicatedAPIServerEndpointPublishing(hostnames map[string]string, oauthLoadBalancer bool) *hyperv1.DedicatedAPIServerEndpointPublishing {
	var dedicated *hyperv1.DedicatedAPIServerEndpointPublishing
	for _, serviceName := range publishingServiceNames[:2] {
		service := hyperv1.DedicatedAPIServerEndpointService{Name: hyperv1.APIServerEndpointServiceName(serviceName), Hostname: hostnames[serviceName]}
		if serviceName == "OAuthServer" && oauthLoadBalancer {
			service.Exposure = hyperv1.ServiceExposureDedicatedLoadBalancer
		}
		if service.Hostname != "" || service.Exposure != "" {
			if dedicated == nil {
				dedicated = &hyperv1.DedicatedAPIServerEndpointPublishing{}
			}
			dedicated.Services = append(dedicated.Services, service)
		}
	}
	return dedicated
}

// nodePortPublishing builds the NodePort preset from the shared address and the per-service ports.
func nodePortPublishing(address string, ports map[string]string) (hyperv1.NodePortPublishing, error) {
	if address == "" {
		return hyperv1.NodePortPublishing{}, fmt.Errorf("NodePort publishing requires --publishing-address or a platform API server address")
	}
	nodePort := hyperv1.NodePortPublishing{Address: address}
	for _, serviceName := range publishingServiceNames {
		portValue, exists := ports[serviceName]
		if !exists {
			continue
		}
		port, err := strconv.ParseInt(portValue, 10, 32)
		if err != nil {
			return hyperv1.NodePortPublishing{}, fmt.Errorf("invalid publishing port for %s: %w", serviceName, err)
		}
		nodePort.Services = append(nodePort.Services, hyperv1.NodePortPublishedService{Name: hyperv1.NodePortServiceName(serviceName), Port: int32(port)})
	}
	return nodePort, nil
}
