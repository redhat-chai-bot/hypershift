package v1beta1

// ServicePublishingType identifies a control plane publishing topology.
// +kubebuilder:validation:Enum=SingleIngressPoint;DedicatedAPIServerEndpoint;NodePort
// +kubebuilder:validation:MaxLength=32
type ServicePublishingType string

const (
	ServicePublishingSingleIngressPoint         ServicePublishingType = "SingleIngressPoint"
	ServicePublishingDedicatedAPIServerEndpoint ServicePublishingType = "DedicatedAPIServerEndpoint"
	ServicePublishingNodePort                   ServicePublishingType = "NodePort"
)

// IngressExposureType identifies how the shared ingress point is exposed.
// +kubebuilder:validation:Enum=LoadBalancer;NodePort;PlatformProvided
// +kubebuilder:validation:MaxLength=16
type IngressExposureType string

const (
	IngressExposureLoadBalancer     IngressExposureType = "LoadBalancer"
	IngressExposureNodePort         IngressExposureType = "NodePort"
	IngressExposurePlatformProvided IngressExposureType = "PlatformProvided"
)

// IngressServiceName identifies a service configurable through the shared ingress point.
// +kubebuilder:validation:Enum=APIServer;OAuthServer;Konnectivity;Ignition
// +kubebuilder:validation:MaxLength=16
type IngressServiceName string

// APIServerEndpointServiceName identifies an endpoint configurable in the dedicated API topology.
// +kubebuilder:validation:Enum=APIServer;OAuthServer
// +kubebuilder:validation:MaxLength=16
type APIServerEndpointServiceName string

// NodePortServiceName identifies a directly published service.
// +kubebuilder:validation:Enum=APIServer;OAuthServer;Konnectivity;Ignition
// +kubebuilder:validation:MaxLength=16
type NodePortServiceName string

// ServiceExposureOverride promotes OAuthServer onto a dedicated Azure self-managed LoadBalancer.
// +kubebuilder:validation:Enum=DedicatedLoadBalancer
// +kubebuilder:validation:MaxLength=24
type ServiceExposureOverride string

const ServiceExposureDedicatedLoadBalancer ServiceExposureOverride = "DedicatedLoadBalancer"

// ServicePublishing configures a supported control plane publishing topology.
// +union
// +kubebuilder:validation:XValidation:rule="self.type == 'SingleIngressPoint' ? has(self.singleIngressPoint) : !has(self.singleIngressPoint)",message="singleIngressPoint must match the publishing type"
// +kubebuilder:validation:XValidation:rule="self.type == 'NodePort' ? has(self.nodePort) : !has(self.nodePort)",message="nodePort must match the publishing type"
// +kubebuilder:validation:XValidation:rule="self.type == 'DedicatedAPIServerEndpoint' || !has(self.dedicatedAPIServerEndpoint)",message="dedicatedAPIServerEndpoint is forbidden for other publishing types"
type ServicePublishing struct {
	// type selects the publishing topology.
	// +unionDiscriminator
	// +required
	Type ServicePublishingType `json:"type,omitempty"`
	// singleIngressPoint configures the shared control plane ingress.
	// +optional
	SingleIngressPoint SingleIngressPointPublishing `json:"singleIngressPoint,omitzero"`
	// dedicatedAPIServerEndpoint configures optional endpoint overrides.
	// An empty object selects the preset without overriding any endpoint, so it is a pointer:
	// the empty object and the absent field are distinct, deliberate user choices.
	// +optional
	DedicatedAPIServerEndpoint *DedicatedAPIServerEndpointPublishing `json:"dedicatedAPIServerEndpoint,omitempty"`
	// nodePort configures direct service exposure on management nodes.
	// +optional
	NodePort NodePortPublishing `json:"nodePort,omitzero"`
}

// SingleIngressPointPublishing publishes services through a shared ingress point.
// +union
// +kubebuilder:validation:XValidation:rule="self.services.exists(s, s.name == 'APIServer' && has(s.hostname))",message="SingleIngressPoint requires an APIServer entry with a hostname"
// +kubebuilder:validation:XValidation:rule="self.services.all(s, !has(s.exposure) || s.name == 'OAuthServer')",message="exposure override is only valid for OAuthServer"
// +kubebuilder:validation:XValidation:rule="self.exposure == 'LoadBalancer' ? has(self.loadBalancer) : !has(self.loadBalancer)",message="loadBalancer must match ingress exposure"
// +kubebuilder:validation:XValidation:rule="self.exposure == 'NodePort' ? has(self.nodePort) : !has(self.nodePort)",message="nodePort must match ingress exposure"
type SingleIngressPointPublishing struct {
	// exposure selects how the shared ingress point is reached.
	// +unionDiscriminator
	// +required
	Exposure IngressExposureType `json:"exposure,omitempty"`
	// loadBalancer configures the router LoadBalancer; an empty object uses its assigned address.
	// +optional
	LoadBalancer *IngressLoadBalancerConfig `json:"loadBalancer,omitempty"`
	// nodePort configures the router NodePort.
	// +optional
	NodePort IngressNodePortConfig `json:"nodePort,omitzero"`
	// services configures optional hostnames and OAuth exposure. APIServer is required.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	// +required
	Services []SingleIngressPointService `json:"services,omitempty"`
}

// SingleIngressPointService configures a service endpoint behind the shared ingress point.
type SingleIngressPointService struct {
	// name identifies the service.
	// +required
	Name IngressServiceName `json:"name,omitempty"`
	// hostname overrides the endpoint DNS name; required for APIServer, otherwise derived when omitted.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule=`self.matches('^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$')`,message="hostname must be a valid DNS subdomain"
	// +optional
	Hostname string `json:"hostname,omitempty"`
	// exposure optionally gives OAuthServer a dedicated LoadBalancer.
	// +optional
	Exposure ServiceExposureOverride `json:"exposure,omitempty"`
}

// IngressLoadBalancerConfig configures optional router DNS metadata.
// The empty object is required by the LoadBalancer exposure and carries meaning on its own,
// so the minimum number of properties is explicitly zero.
// +kubebuilder:validation:MinProperties=0
type IngressLoadBalancerConfig struct {
	// hostname overrides the router LoadBalancer DNS name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule=`self.matches('^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$')`,message="hostname must be a valid DNS subdomain"
	// +optional
	Hostname string `json:"hostname,omitempty"`
}

// IngressNodePortConfig configures a router endpoint on management nodes.
type IngressNodePortConfig struct {
	// address is a reachable hostname, IPv4, or IPv6 address.
	// +kubebuilder:validation:XValidation:rule=`isIP(self) || (self.lowerAscii().contains(':ffff:') && isIP(self.lowerAscii().replace(':ffff:', ':fffe:'))) || (self.matches('^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$') && !self.matches('^[0-9]+(\\.[0-9]+){3}$'))`, message="address must be a valid hostname, IPv4, or IPv6 address"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Address string `json:"address,omitempty"`
	// port selects a NodePort; omitted ports are allocated by Kubernetes.
	// +kubebuilder:validation:Minimum=30000
	// +kubebuilder:validation:Maximum=32767
	// +optional
	Port int32 `json:"port,omitempty"`
}

// DedicatedAPIServerEndpointPublishing publishes the API server on its own LoadBalancer.
// An empty object is a deliberate choice: it selects the preset without overriding any endpoint,
// so the minimum number of properties is explicitly zero.
// +kubebuilder:validation:MinProperties=0
// +kubebuilder:validation:XValidation:rule="!has(self.services) || self.services.all(s, !has(s.exposure) || s.name == 'OAuthServer')",message="exposure override is only valid for OAuthServer"
type DedicatedAPIServerEndpointPublishing struct {
	// services configures endpoint overrides. Omitted endpoints use their assigned or derived names.
	// An empty list is equivalent to omitting the field, so the minimum number of items is explicitly zero.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=0
	// +kubebuilder:validation:MaxItems=2
	// +optional
	Services []DedicatedAPIServerEndpointService `json:"services,omitempty"`
}

// DedicatedAPIServerEndpointService configures an API or OAuth endpoint override.
type DedicatedAPIServerEndpointService struct {
	// name identifies the service.
	// +required
	Name APIServerEndpointServiceName `json:"name,omitempty"`
	// hostname overrides the DNS name; omission preserves assigned or derived names.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:XValidation:rule=`self.matches('^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$')`,message="hostname must be a valid DNS subdomain"
	// +optional
	Hostname string `json:"hostname,omitempty"`
	// exposure optionally gives OAuthServer a dedicated LoadBalancer.
	// +optional
	Exposure ServiceExposureOverride `json:"exposure,omitempty"`
}

// NodePortPublishing publishes services directly at a shared management node address.
// +kubebuilder:validation:XValidation:rule="!has(self.services) || self.services.all(x, !has(x.port) || self.services.filter(y, has(y.port) && y.port == x.port).size() == 1)",message="explicit port values must be unique across services"
type NodePortPublishing struct {
	// address is a reachable hostname, IPv4, or IPv6 address.
	// +kubebuilder:validation:XValidation:rule=`isIP(self) || (self.lowerAscii().contains(':ffff:') && isIP(self.lowerAscii().replace(':ffff:', ':fffe:'))) || (self.matches('^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$') && !self.matches('^[0-9]+(\\.[0-9]+){3}$'))`, message="address must be a valid hostname, IPv4, or IPv6 address"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Address string `json:"address,omitempty"`
	// services configures optional per-service port overrides.
	// An empty list is equivalent to omitting the field, so the minimum number of items is explicitly zero.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=0
	// +kubebuilder:validation:MaxItems=4
	// +optional
	Services []NodePortPublishedService `json:"services,omitempty"`
}

// NodePortPublishedService selects an optional NodePort for a service.
type NodePortPublishedService struct {
	// name identifies the service.
	// +required
	Name NodePortServiceName `json:"name,omitempty"`
	// port selects a NodePort; omitted ports are allocated by Kubernetes.
	// +kubebuilder:validation:Minimum=30000
	// +kubebuilder:validation:Maximum=32767
	// +optional
	Port int32 `json:"port,omitempty"`
}
