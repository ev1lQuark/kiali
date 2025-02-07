package kubernetes

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	api_networking_v1beta1 "istio.io/api/networking/v1beta1"
	networking_v1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	security_v1beta1 "istio.io/client-go/pkg/apis/security/v1beta1"
	istio "istio.io/client-go/pkg/clientset/versioned"
	core_v1 "k8s.io/api/core/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	k8s_networking_v1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	gatewayapiclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/log"
	"github.com/kiali/kiali/util/httputil"
)

const (
	ComponentHealthy     = "Healthy"
	ComponentNotFound    = "NotFound"
	ComponentNotReady    = "NotReady"
	ComponentUnhealthy   = "Unhealthy"
	ComponentUnreachable = "Unreachable"
)

type ComponentStatus struct {
	// Namespace where the component is deployed.
	// This field is ignored when marshalling to JSON.
	Namespace string `json:"-"`

	// The workload name of the Istio component.
	//
	// example: istio-ingressgateway
	// required: true
	Name string `json:"name"`

	// The status of an Istio component.
	//
	// example:  Not Found
	// required: true
	Status string `json:"status"`

	// When true, the component is necessary for Istio to function. Otherwise, it is an addon.
	//
	// example:  true
	// required: true
	IsCore bool `json:"is_core"`
}

type IstioComponentStatus []ComponentStatus

func (ics *IstioComponentStatus) Merge(cs IstioComponentStatus) IstioComponentStatus {
	*ics = append(*ics, cs...)
	return *ics
}

const (
	envoyAdminPort = 15000
)

var (
	portNameMatcher = regexp.MustCompile(`^[\-].*`)
	// UDP protocol is not proxied, but it is functional. keeping it in protocols list not to cause UI issues.
	portProtocols = [...]string{"grpc", "grpc-web", "http", "http2", "https", "mongo", "redis", "tcp", "tls", "udp", "mysql"}
)

type IstioClientInterface interface {
	Istio() istio.Interface
	// GatewayAPI returns the gateway-api kube client.
	GatewayAPI() gatewayapiclient.Interface

	GetConfigDump(namespace, podName string) (*ConfigDump, error)
	SetProxyLogLevel(namespace, podName, level string) error
}

func (in *K8SClient) Istio() istio.Interface {
	return in.istioClientset
}

func (in *K8SClient) GatewayAPI() gatewayapiclient.Interface {
	return in.gatewayapi
}

func (in *K8SClient) GetConfigDump(namespace, podName string) (*ConfigDump, error) {
	// Fetching the Config Dump from the pod's Envoy.
	// The port 15000 is open on each Envoy Sidecar (managed by Istio) to serve the Envoy Admin  interface.
	// This port can only be accessed by inside the pod.
	// See the Istio's doc page about its port usage:
	// https://istio.io/latest/docs/ops/deployment/requirements/#ports-used-by-istio
	resp, err := in.ForwardGetRequest(namespace, podName, 15000, "/config_dump")
	if err != nil {
		log.Errorf("Error forwarding the /config_dump request: %v", err)
		return nil, err
	}

	cd := &ConfigDump{}
	err = json.Unmarshal(resp, cd)
	if err != nil {
		log.Errorf("Error Unmarshalling the config_dump: %v", err)
	}

	return cd, err
}

func (in *K8SClient) SetProxyLogLevel(namespace, pod, level string) error {
	path := fmt.Sprintf("/logging?level=%s", level)

	localPort := httputil.Pool.GetFreePort()
	defer httputil.Pool.FreePort(localPort)
	f, err := in.getPodPortForwarder(namespace, pod, fmt.Sprintf("%d:%d", localPort, envoyAdminPort))
	if err != nil {
		return err
	}

	// Start the forwarding
	if err := f.Start(); err != nil {
		return err
	}

	// Defering the finish of the port-forwarding
	defer f.Stop()

	// Ready to create a request
	url := fmt.Sprintf("http://localhost:%d%s", localPort, path)
	body, code, _, err := httputil.HttpPost(url, nil, nil, time.Second*10, nil)
	if code >= 400 {
		log.Errorf("Error whilst posting. Error: %s. Body: %s", err, string(body))
		return fmt.Errorf("error sending post request %s from %s/%s. Response code: %d", path, namespace, pod, code)
	}

	return err
}

func GetIstioConfigMap(istioConfig *core_v1.ConfigMap) (*IstioMeshConfig, error) {
	meshConfig := &IstioMeshConfig{}

	// Used for test cases
	if istioConfig == nil || istioConfig.Data == nil {
		return meshConfig, nil
	}

	var err error
	meshConfigYaml, ok := istioConfig.Data["mesh"]
	log.Tracef("meshConfig: %v", meshConfigYaml)
	if !ok {
		errMsg := "GetIstioConfigMap: Cannot find Istio mesh configuration [%v]."
		log.Warningf(errMsg, istioConfig)
		return nil, fmt.Errorf(errMsg, istioConfig)
	}

	err = k8syaml.Unmarshal([]byte(meshConfigYaml), &meshConfig)
	if err != nil {
		log.Warningf("GetIstioConfigMap: Cannot read Istio mesh configuration.")
		return nil, err
	}
	return meshConfig, nil
}

// ServiceEntryHostnames returns a list of hostnames defined in the ServiceEntries Specs. Key in the resulting map is the protocol (in lowercase) + hostname
// exported for test
func ServiceEntryHostnames(serviceEntries []*networking_v1beta1.ServiceEntry) map[string][]string {
	hostnames := make(map[string][]string)

	for _, v := range serviceEntries {
		for _, host := range v.Spec.Hosts {
			hostnames[host] = make([]string, 0, 1)
		}
		for _, port := range v.Spec.Ports {
			protocol := mapPortToVirtualServiceProtocol(port.Protocol)
			for host := range hostnames {
				hostnames[host] = append(hostnames[host], protocol)
			}
		}
	}
	return hostnames
}

// mapPortToVirtualServiceProtocol transforms Istio's Port-definitions' protocol names to VirtualService's protocol names
func mapPortToVirtualServiceProtocol(proto string) string {
	// http: HTTP/HTTP2/GRPC/ TLS-terminated-HTTPS and service entry ports using HTTP/HTTP2/GRPC protocol
	// tls: HTTPS/TLS protocols (i.e. with “passthrough” TLS mode) and service entry ports using HTTPS/TLS protocols.
	// tcp: everything else

	switch proto {
	case "HTTP":
		fallthrough
	case "HTTP2":
		fallthrough
	case "GRPC":
		return "http"
	case "HTTPS":
		fallthrough
	case "TLS":
		return "tls"
	default:
		return "tcp"
	}
}

// ValidatePort parses the Istio Port definition and validates the naming scheme
func ValidatePort(portDef *api_networking_v1beta1.Port) bool {
	if portDef == nil {
		return false
	}
	return MatchPortNameRule(portDef.Name, portDef.Protocol)
}

// ValidateServicePort parses the Istio Port definition and validates the naming scheme
func ValidateServicePort(portDef *api_networking_v1beta1.ServicePort) bool {
	if portDef == nil {
		return false
	}
	return MatchPortNameRule(portDef.Name, portDef.Protocol)
}

func MatchPortNameRule(portName, protocol string) bool {
	protocol = strings.ToLower(protocol)
	// Check that portName begins with the protocol
	if protocol == "tcp" || protocol == "udp" {
		// TCP and UDP protocols do not care about the name
		return true
	}

	if !strings.HasPrefix(portName, protocol) {
		return false
	}

	// If longer than protocol, then it must adhere to <protocol>[-suffix]
	// and if there's -, then there must be a suffix ..
	if len(portName) > len(protocol) {
		restPortName := portName[len(protocol):]
		return portNameMatcher.MatchString(restPortName)
	}

	// Case portName == protocolName
	return true
}

func MatchPortNameWithValidProtocols(portName string) bool {
	for _, protocol := range portProtocols {
		if strings.HasPrefix(portName, protocol) &&
			(strings.ToLower(portName) == protocol || portNameMatcher.MatchString(portName[len(protocol):])) {
			return true
		}
	}
	return false
}

func MatchPortAppProtocolWithValidProtocols(appProtocol *string) bool {
	if appProtocol == nil || *appProtocol == "" {
		return false
	}
	for _, protocol := range portProtocols {
		if strings.ToLower(*appProtocol) == protocol {
			return true
		}
	}
	return false
}

// GatewayNames extracts the gateway names for easier matching
func GatewayNames(gateways []*networking_v1beta1.Gateway) map[string]struct{} {
	var empty struct{}
	names := make(map[string]struct{})
	for _, gw := range gateways {
		names[ParseHost(gw.Name, gw.Namespace).String()] = empty
	}
	return names
}

// K8sGatewayNames extracts the gateway names for easier matching
func K8sGatewayNames(gateways []*k8s_networking_v1beta1.Gateway) map[string]struct{} {
	var empty struct{}
	names := make(map[string]struct{})
	for _, gw := range gateways {
		names[ParseHost(gw.Name, gw.Namespace).String()] = empty
	}
	return names
}

func PeerAuthnHasStrictMTLS(peerAuthn *security_v1beta1.PeerAuthentication) bool {
	_, mode := PeerAuthnHasMTLSEnabled(peerAuthn)
	return mode == "STRICT"
}

func PeerAuthnHasMTLSEnabled(peerAuthn *security_v1beta1.PeerAuthentication) (bool, string) {
	// It is no globally enabled when has targets
	if peerAuthn.Spec.Selector != nil && len(peerAuthn.Spec.Selector.MatchLabels) >= 0 {
		return false, ""
	}
	return PeerAuthnMTLSMode(peerAuthn)
}

func PeerAuthnMTLSMode(peerAuthn *security_v1beta1.PeerAuthentication) (bool, string) {
	// It is globally enabled when mtls is in STRICT mode
	if peerAuthn.Spec.Mtls != nil {
		mode := peerAuthn.Spec.Mtls.Mode.String()
		return mode == "STRICT" || mode == "PERMISSIVE", mode
	}
	return false, ""
}

func DestinationRuleHasMeshWideMTLSEnabled(destinationRule *networking_v1beta1.DestinationRule) (bool, string) {
	// Following the suggested procedure to enable mesh-wide mTLS, host might be '*.local':
	// https://istio.io/docs/tasks/security/authn-policy/#globally-enabling-istio-mutual-tls
	return DestinationRuleHasMTLSEnabledForHost("*.local", destinationRule)
}

func DestinationRuleHasNamespaceWideMTLSEnabled(namespace string, destinationRule *networking_v1beta1.DestinationRule) (bool, string) {
	// Following the suggested procedure to enable namespace-wide mTLS, host might be '*.namespace.svc.cluster.local'
	// https://istio.io/docs/tasks/security/authn-policy/#namespace-wide-policy
	nsHost := fmt.Sprintf("*.%s.%s", namespace, config.Get().ExternalServices.Istio.IstioIdentityDomain)
	return DestinationRuleHasMTLSEnabledForHost(nsHost, destinationRule)
}

func DestinationRuleHasMTLSEnabledForHost(expectedHost string, destinationRule *networking_v1beta1.DestinationRule) (bool, string) {
	if destinationRule.Spec.Host == "" || destinationRule.Spec.Host != expectedHost {
		return false, ""
	}
	return DestinationRuleHasMTLSEnabled(destinationRule)
}

func DestinationRuleHasMTLSEnabled(destinationRule *networking_v1beta1.DestinationRule) (bool, string) {
	if destinationRule.Spec.TrafficPolicy != nil && destinationRule.Spec.TrafficPolicy.Tls != nil {
		mode := destinationRule.Spec.TrafficPolicy.Tls.Mode.String()
		return mode == "ISTIO_MUTUAL", mode
	}
	return false, ""
}

// ClusterInfoFromIstiod attempts to resolve the cluster info of the "home" cluster where kiali is running
// by inspecting the istiod deployment. Assumes that the istiod deployment is in the same cluster as the kiali pod.
func ClusterInfoFromIstiod(conf config.Config, k8s ClientInterface) (string, bool, error) {
	// The "cluster_id" is set in an environment variable of
	// the "istiod" deployment. Let's try to fetch it.
	istioDeploymentConfig := conf.ExternalServices.Istio.IstiodDeploymentName
	istiodDeployment, err := k8s.GetDeployment(conf.IstioNamespace, istioDeploymentConfig)
	if err != nil {
		return "", false, err
	}

	istiodContainers := istiodDeployment.Spec.Template.Spec.Containers
	if len(istiodContainers) == 0 {
		return "", false, fmt.Errorf("istiod deployment [%s] has no containers", istioDeploymentConfig)
	}

	clusterName := ""
	for _, v := range istiodContainers[0].Env {
		if v.Name == "CLUSTER_ID" {
			clusterName = v.Value
			break
		}
	}

	gatewayToNamespace := false
	for _, v := range istiodContainers[0].Env {
		if v.Name == "PILOT_SCOPE_GATEWAY_TO_NAMESPACE" {
			gatewayToNamespace = v.Value == "true"
			break
		}
	}

	if clusterName == "" {
		// We didn't find it. This may mean that Istio is not setup with multi-cluster enabled.
		return "", false, fmt.Errorf("istiod deployment [%s] does not have the CLUSTER_ID environment variable set", istioDeploymentConfig)
	}

	return clusterName, gatewayToNamespace, nil
}

func GatewayAPIClasses() []config.GatewayAPIClass {
	result := []config.GatewayAPIClass{}
	for _, gwClass := range config.Get().ExternalServices.Istio.GatewayAPIClasses {
		if gwClass.ClassName != "" && gwClass.Name != "" {
			result = append(result, gwClass)
		}
	}
	if len(result) == 0 {
		result = append(result, config.GatewayAPIClass{Name: "Istio", ClassName: "istio"})
	}
	return result
}
