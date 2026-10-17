package publishing

import (
	"fmt"
	"reflect"

	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	"k8s.io/apimachinery/pkg/util/sets"
)

var serviceOrder = []hyperv1.ServiceType{hyperv1.APIServer, hyperv1.OAuthServer, hyperv1.Konnectivity, hyperv1.Ignition}

func ServicesForHC(hc *hyperv1.HostedCluster) ([]hyperv1.ServicePublishingStrategyMapping, error) {
	if hc.Spec.Publishing.Type == "" {
		return copyServices(hc.Spec.Services), nil
	}
	return ToServices(&hc.Spec.Publishing, hc.Spec.Platform)
}

func ServicesForHCP(hcp *hyperv1.HostedControlPlane) ([]hyperv1.ServicePublishingStrategyMapping, error) {
	if len(hcp.Spec.Services) > 0 || hcp.Spec.Publishing.Type == "" {
		return copyServices(hcp.Spec.Services), nil
	}
	return ToServices(&hcp.Spec.Publishing, hcp.Spec.Platform)
}

func HCPTranslationError(hcp *hyperv1.HostedControlPlane) error {
	if hcp.Spec.Publishing.Type == "" || len(hcp.Spec.Services) == 0 {
		return nil
	}
	translated, err := ToServices(&hcp.Spec.Publishing, hcp.Spec.Platform)
	if err != nil {
		return err
	}
	return EquivalentServices(hcp.Spec.Services, translated)
}

func ToServices(policy *hyperv1.ServicePublishing, platform hyperv1.PlatformSpec) ([]hyperv1.ServicePublishingStrategyMapping, error) {
	if policy == nil {
		return nil, fmt.Errorf("publishing policy is required")
	}
	if err := validatePlatform(policy, platform); err != nil {
		return nil, err
	}
	services := make(map[hyperv1.ServiceType]hyperv1.ServicePublishingStrategy)
	for _, name := range serviceOrder {
		if name == hyperv1.Ignition && platform.Type == hyperv1.IBMCloudPlatform {
			continue
		}
		services[name] = hyperv1.ServicePublishingStrategy{Type: hyperv1.Route}
	}
	seen := sets.New[hyperv1.ServiceType]()
	switch policy.Type {
	case hyperv1.ServicePublishingSingleIngressPoint:
		if err := applySingleIngressPoint(policy.SingleIngressPoint, services, seen, platform); err != nil {
			return nil, err
		}
	case hyperv1.ServicePublishingDedicatedAPIServerEndpoint:
		if err := applyDedicatedAPIServerEndpoint(policy.DedicatedAPIServerEndpoint, services, seen, platform); err != nil {
			return nil, err
		}
	case hyperv1.ServicePublishingNodePort:
		if err := applyNodePort(policy.NodePort, services, seen); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown publishing type %q", policy.Type)
	}
	result := make([]hyperv1.ServicePublishingStrategyMapping, 0, len(services))
	for _, name := range serviceOrder {
		if strategy, present := services[name]; present {
			result = append(result, hyperv1.ServicePublishingStrategyMapping{Service: name, ServicePublishingStrategy: strategy})
		}
	}
	return result, nil
}

// applySingleIngressPoint projects the SingleIngressPoint preset onto the legacy strategies.
func applySingleIngressPoint(ingress hyperv1.SingleIngressPointPublishing, services map[hyperv1.ServiceType]hyperv1.ServicePublishingStrategy, seen sets.Set[hyperv1.ServiceType], platform hyperv1.PlatformSpec) error {
	switch ingress.Exposure {
	case hyperv1.IngressExposureLoadBalancer:
		if ingress.LoadBalancer == nil {
			return fmt.Errorf("SingleIngressPoint LoadBalancer requires loadBalancer configuration")
		}
	case hyperv1.IngressExposurePlatformProvided:
	case hyperv1.IngressExposureNodePort:
		if ingress.NodePort.Address == "" {
			return fmt.Errorf("SingleIngressPoint NodePort requires an address")
		}
		if ingress.NodePort.Port != 0 && (ingress.NodePort.Port < 30000 || ingress.NodePort.Port > 32767) {
			return fmt.Errorf("router NodePort must be between 30000 and 32767")
		}
	default:
		return fmt.Errorf("unknown ingress exposure %q", ingress.Exposure)
	}
	for _, entry := range ingress.Services {
		name := hyperv1.ServiceType(entry.Name)
		if err := checkEntry(name, seen, false); err != nil {
			return err
		}
		strategy, err := hostnameStrategy(name, entry.Hostname, string(entry.Exposure), hyperv1.Route, platform)
		if err != nil {
			return err
		}
		services[name] = strategy
	}
	api := services[hyperv1.APIServer]
	if api.Route == nil || api.Route.Hostname == "" {
		return fmt.Errorf("SingleIngressPoint requires an APIServer hostname")
	}
	return nil
}

// applyDedicatedAPIServerEndpoint projects the DedicatedAPIServerEndpoint preset, whose arm may be
// absent because every endpoint keeps its assigned or derived name.
func applyDedicatedAPIServerEndpoint(dedicated *hyperv1.DedicatedAPIServerEndpointPublishing, services map[hyperv1.ServiceType]hyperv1.ServicePublishingStrategy, seen sets.Set[hyperv1.ServiceType], platform hyperv1.PlatformSpec) error {
	services[hyperv1.APIServer] = hyperv1.ServicePublishingStrategy{Type: hyperv1.LoadBalancer}
	if dedicated == nil {
		return nil
	}
	for _, entry := range dedicated.Services {
		name := hyperv1.ServiceType(entry.Name)
		if err := checkEntry(name, seen, true); err != nil {
			return err
		}
		strategy, err := hostnameStrategy(name, entry.Hostname, string(entry.Exposure), services[name].Type, platform)
		if err != nil {
			return err
		}
		services[name] = strategy
	}
	return nil
}

// applyNodePort projects the NodePort preset, giving every service the shared address and the
// explicit ports the policy requests.
func applyNodePort(nodePort hyperv1.NodePortPublishing, services map[hyperv1.ServiceType]hyperv1.ServicePublishingStrategy, seen sets.Set[hyperv1.ServiceType]) error {
	if nodePort.Address == "" {
		return fmt.Errorf("NodePort publishing requires an address")
	}
	for name := range services {
		services[name] = hyperv1.ServicePublishingStrategy{Type: hyperv1.NodePort, NodePort: &hyperv1.NodePortPublishingStrategy{Address: nodePort.Address}}
	}
	ports := sets.New[int32]()
	for _, entry := range nodePort.Services {
		name := hyperv1.ServiceType(entry.Name)
		if err := checkEntry(name, seen, false); err != nil {
			return err
		}
		port := entry.Port
		if port != 0 && (port < 30000 || port > 32767 || ports.Has(port)) {
			return fmt.Errorf("service %s has an invalid or duplicate NodePort %d", name, port)
		}
		ports.Insert(port)
		services[name] = hyperv1.ServicePublishingStrategy{Type: hyperv1.NodePort, NodePort: &hyperv1.NodePortPublishingStrategy{Address: nodePort.Address, Port: port}}
	}
	return nil
}

func FromServices(services []hyperv1.ServicePublishingStrategyMapping, platform hyperv1.PlatformSpec) (*hyperv1.ServicePublishing, error) {
	byName := make(map[hyperv1.ServiceType]hyperv1.ServicePublishingStrategy)
	seen := sets.New[hyperv1.ServiceType]()
	for _, service := range services {
		if err := checkEntry(service.Service, seen, false); err != nil {
			return nil, err
		}
		byName[service.Service] = service.ServicePublishingStrategy
	}
	for _, name := range serviceOrder {
		if _, present := byName[name]; !present && (name != hyperv1.Ignition || platform.Type != hyperv1.IBMCloudPlatform) {
			return nil, fmt.Errorf("legacy services are missing %s", name)
		}
	}
	policy := &hyperv1.ServicePublishing{}
	switch byName[hyperv1.APIServer].Type {
	case hyperv1.Route:
		policy.Type = hyperv1.ServicePublishingSingleIngressPoint
		policy.SingleIngressPoint.Exposure = hyperv1.IngressExposureLoadBalancer
		policy.SingleIngressPoint.LoadBalancer = &hyperv1.IngressLoadBalancerConfig{}
		if platform.Type == hyperv1.IBMCloudPlatform || isManagedAzure(platform) {
			policy.SingleIngressPoint.Exposure = hyperv1.IngressExposurePlatformProvided
			policy.SingleIngressPoint.LoadBalancer = nil
		}
	case hyperv1.LoadBalancer:
		policy.Type = hyperv1.ServicePublishingDedicatedAPIServerEndpoint
		if platform.Type == hyperv1.AWSPlatform && platform.AWS != nil && platform.AWS.EndpointAccess == hyperv1.Private {
			return nil, fmt.Errorf("AWS Private with a dedicated API load balancer has no publishing preset")
		}
	case hyperv1.NodePort:
		policy.Type = hyperv1.ServicePublishingNodePort
	default:
		return nil, fmt.Errorf("APIServer strategy %q has no publishing preset", byName[hyperv1.APIServer].Type)
	}
	for _, name := range serviceOrder {
		strategy, present := byName[name]
		if !present {
			continue
		}
		if policy.Type == hyperv1.ServicePublishingNodePort {
			if err := collectNodePortEntry(policy, name, strategy, platform); err != nil {
				return nil, err
			}
			continue
		}
		collectHostnameEntry(policy, name, strategy)
	}
	translated, err := ToServices(policy, platform)
	if err != nil {
		return nil, err
	}
	if err := EquivalentServices(services, translated); err != nil {
		return nil, fmt.Errorf("legacy topology cannot be represented without loss: %w", err)
	}
	return policy, nil
}

// collectNodePortEntry folds one legacy NodePort strategy into the NodePort preset, rejecting
// topologies the preset cannot express.
func collectNodePortEntry(policy *hyperv1.ServicePublishing, name hyperv1.ServiceType, strategy hyperv1.ServicePublishingStrategy, platform hyperv1.PlatformSpec) error {
	if strategy.Type != hyperv1.NodePort || strategy.NodePort == nil {
		return fmt.Errorf("service %s does not use NodePort", name)
	}
	if strategy.NodePort.Address == "" {
		return fmt.Errorf("service %s has an empty NodePort address", name)
	}
	if policy.NodePort.Address == "" {
		policy.NodePort.Address = strategy.NodePort.Address
	} else if policy.NodePort.Address != strategy.NodePort.Address {
		return fmt.Errorf("NodePort services have different addresses")
	}
	// IBMCloud omits Ignition from the preset default set, so it has to be listed explicitly even
	// when its port is dynamically assigned.
	if strategy.NodePort.Port != 0 || (name == hyperv1.Ignition && platform.Type == hyperv1.IBMCloudPlatform) {
		policy.NodePort.Services = append(policy.NodePort.Services, hyperv1.NodePortPublishedService{Name: hyperv1.NodePortServiceName(name), Port: strategy.NodePort.Port})
	}
	return nil
}

// collectHostnameEntry folds one legacy Route or LoadBalancer strategy into the hostname-carrying
// presets.
func collectHostnameEntry(policy *hyperv1.ServicePublishing, name hyperv1.ServiceType, strategy hyperv1.ServicePublishingStrategy) {
	hostname := ""
	exposure := hyperv1.ServiceExposureOverride("")
	switch {
	case strategy.Type == hyperv1.Route && strategy.Route != nil:
		hostname = strategy.Route.Hostname
	case strategy.Type == hyperv1.LoadBalancer:
		if strategy.LoadBalancer != nil {
			hostname = strategy.LoadBalancer.Hostname
		}
		if name == hyperv1.OAuthServer {
			exposure = hyperv1.ServiceExposureDedicatedLoadBalancer
		}
	}
	if policy.Type == hyperv1.ServicePublishingSingleIngressPoint {
		policy.SingleIngressPoint.Services = append(policy.SingleIngressPoint.Services, hyperv1.SingleIngressPointService{Name: hyperv1.IngressServiceName(name), Hostname: hostname, Exposure: exposure})
		return
	}
	if name != hyperv1.APIServer && name != hyperv1.OAuthServer {
		return
	}
	if hostname == "" && exposure == "" {
		return
	}
	if policy.DedicatedAPIServerEndpoint == nil {
		policy.DedicatedAPIServerEndpoint = &hyperv1.DedicatedAPIServerEndpointPublishing{}
	}
	policy.DedicatedAPIServerEndpoint.Services = append(policy.DedicatedAPIServerEndpoint.Services, hyperv1.DedicatedAPIServerEndpointService{Name: hyperv1.APIServerEndpointServiceName(name), Hostname: hostname, Exposure: exposure})
}

func EquivalentServices(original, translated []hyperv1.ServicePublishingStrategyMapping) error {
	if len(original) != len(translated) {
		return fmt.Errorf("service count differs (%d and %d)", len(original), len(translated))
	}
	byName := make(map[hyperv1.ServiceType]hyperv1.ServicePublishingStrategy)
	for _, service := range original {
		if _, present := byName[service.Service]; present {
			return fmt.Errorf("duplicate legacy service %s", service.Service)
		}
		byName[service.Service] = normalizeStrategy(service.ServicePublishingStrategy)
	}
	for _, service := range translated {
		originalStrategy, present := byName[service.Service]
		if !present || !reflect.DeepEqual(originalStrategy, normalizeStrategy(service.ServicePublishingStrategy)) {
			return fmt.Errorf("service %s strategy or endpoint metadata differs", service.Service)
		}
		delete(byName, service.Service)
	}
	if len(byName) != 0 {
		return fmt.Errorf("translated service names differ")
	}
	return nil
}

func hostnameStrategy(name hyperv1.ServiceType, hostname, exposure string, defaultType hyperv1.PublishingStrategyType, platform hyperv1.PlatformSpec) (hyperv1.ServicePublishingStrategy, error) {
	strategy := hyperv1.ServicePublishingStrategy{Type: defaultType}
	if exposure != "" {
		if exposure != string(hyperv1.ServiceExposureDedicatedLoadBalancer) || name != hyperv1.OAuthServer || platform.Type != hyperv1.AzurePlatform || isManagedAzure(platform) {
			return strategy, fmt.Errorf("service %s exposure %q requires Azure self-managed OAuthServer", name, exposure)
		}
		strategy.Type = hyperv1.LoadBalancer
	}
	if hostname != "" {
		if strategy.Type == hyperv1.LoadBalancer {
			strategy.LoadBalancer = &hyperv1.LoadBalancerPublishingStrategy{Hostname: hostname}
		} else {
			strategy.Route = &hyperv1.RoutePublishingStrategy{Hostname: hostname}
		}
	}
	return strategy, nil
}

func checkEntry(name hyperv1.ServiceType, seen sets.Set[hyperv1.ServiceType], dedicated bool) error {
	switch name {
	case hyperv1.APIServer, hyperv1.OAuthServer:
	case hyperv1.Konnectivity, hyperv1.Ignition:
		if dedicated {
			return fmt.Errorf("service %s is not configurable in DedicatedAPIServerEndpoint", name)
		}
	default:
		return fmt.Errorf("service %s has no publishing preset", name)
	}
	if seen.Has(name) {
		return fmt.Errorf("duplicate service %s", name)
	}
	seen.Insert(name)
	return nil
}

func isManagedAzure(platform hyperv1.PlatformSpec) bool {
	return platform.Type == hyperv1.AzurePlatform && platform.Azure != nil && platform.Azure.AzureAuthenticationConfig.AzureAuthenticationConfigType == hyperv1.AzureAuthenticationTypeManagedIdentities
}

func validatePlatform(policy *hyperv1.ServicePublishing, platform hyperv1.PlatformSpec) error {
	allowed := false
	switch policy.Type {
	case hyperv1.ServicePublishingSingleIngressPoint:
		switch platform.Type {
		case hyperv1.AWSPlatform, hyperv1.AzurePlatform, hyperv1.GCPPlatform, hyperv1.KubevirtPlatform, hyperv1.PowerVSPlatform, hyperv1.IBMCloudPlatform, hyperv1.AgentPlatform, hyperv1.NonePlatform:
			allowed = true
		}
		if platform.Type == hyperv1.IBMCloudPlatform && policy.SingleIngressPoint.Exposure != hyperv1.IngressExposurePlatformProvided {
			allowed = false
		}
		if policy.SingleIngressPoint.Exposure == hyperv1.IngressExposureNodePort && platform.Type != hyperv1.AgentPlatform && platform.Type != hyperv1.KubevirtPlatform && platform.Type != hyperv1.NonePlatform {
			allowed = false
		}
	case hyperv1.ServicePublishingDedicatedAPIServerEndpoint:
		switch platform.Type {
		case hyperv1.AWSPlatform, hyperv1.AzurePlatform, hyperv1.GCPPlatform, hyperv1.KubevirtPlatform, hyperv1.PowerVSPlatform, hyperv1.AgentPlatform, hyperv1.NonePlatform, hyperv1.OpenStackPlatform:
			allowed = true
		}
		if platform.Type == hyperv1.AWSPlatform && platform.AWS != nil && platform.AWS.EndpointAccess == hyperv1.Private {
			allowed = false
		}
		if platform.Type == hyperv1.AzurePlatform && platform.Azure != nil && (platform.Azure.Topology == hyperv1.AzureTopologyPrivate || isManagedAzure(platform)) {
			allowed = false
		}
	case hyperv1.ServicePublishingNodePort:
		switch platform.Type {
		case hyperv1.AgentPlatform, hyperv1.KubevirtPlatform, hyperv1.NonePlatform, hyperv1.IBMCloudPlatform:
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("publishing topology %s is unsupported on platform %s with the configured endpoint access", policy.Type, platform.Type)
	}
	return nil
}

func normalizeStrategy(strategy hyperv1.ServicePublishingStrategy) hyperv1.ServicePublishingStrategy {
	strategy = *strategy.DeepCopy()
	if strategy.Route != nil && *strategy.Route == (hyperv1.RoutePublishingStrategy{}) {
		strategy.Route = nil
	}
	if strategy.LoadBalancer != nil && *strategy.LoadBalancer == (hyperv1.LoadBalancerPublishingStrategy{}) {
		strategy.LoadBalancer = nil
	}
	return strategy
}

func copyServices(services []hyperv1.ServicePublishingStrategyMapping) []hyperv1.ServicePublishingStrategyMapping {
	if services == nil {
		return nil
	}
	result := make([]hyperv1.ServicePublishingStrategyMapping, len(services))
	for index := range services {
		result[index] = *services[index].DeepCopy()
	}
	return result
}
