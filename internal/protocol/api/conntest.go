package api

// This file holds 04's connection-test types (04 §7.7) and the value sets that netx, doctor and the SPA share:
// Transport, CloudProvider (04 §13.3), NATKind (04 §7.4) and ContainerKind.

// Transport is one way media reaches the server: the ICE transports of 04 §7.3. It is the conntest request's
// transport, AdvertisedAddr.Via, ConnectionLive.Transport and a key of TransportCounts.
type Transport string

const (
	TransportUDP     Transport = "udp"     // UDP mux on listen.ice_udp (7882)
	TransportTCP443  Transport = "tcp443"  // ICE-TCP through the 443 first-byte multiplexer
	TransportTCP7882 Transport = "tcp7882" // ICE-TCP on listen.ice_tcp (7882)
)

// CloudProvider is a hosting provider detected from DMI data (04 §13.3). 05 has a fix.firewall.<provider> text for
// each value, and 06's site an install/vps#<provider> anchor.
type CloudProvider string

const (
	CloudProviderAWS          CloudProvider = "aws"
	CloudProviderGCP          CloudProvider = "gcp"
	CloudProviderAzure        CloudProvider = "azure"
	CloudProviderOracle       CloudProvider = "oracle"
	CloudProviderHetzner      CloudProvider = "hetzner"
	CloudProviderDigitalOcean CloudProvider = "digitalocean"
	CloudProviderVultr        CloudProvider = "vultr"
	CloudProviderLinode       CloudProvider = "linode"
	CloudProviderScaleway     CloudProvider = "scaleway"
	CloudProviderOVH          CloudProvider = "ovh"
	CloudProviderAlibaba      CloudProvider = "alibaba"
	CloudProviderTencent      CloudProvider = "tencent"
	CloudProviderUnknown      CloudProvider = "unknown"
)

// CloudProviders returns every CloudProvider value in the order of 04 §13.3.
func CloudProviders() []CloudProvider {
	return []CloudProvider{
		CloudProviderAWS, CloudProviderGCP, CloudProviderAzure, CloudProviderOracle, CloudProviderHetzner,
		CloudProviderDigitalOcean, CloudProviderVultr, CloudProviderLinode, CloudProviderScaleway, CloudProviderOVH,
		CloudProviderAlibaba, CloudProviderTencent, CloudProviderUnknown,
	}
}

// NATKind classifies the server's public IPv4 path (04 §7.4). 05 has a conntest.nat.<nat> text for each value.
type NATKind string

const (
	NATKindNone        NATKind = "none"         // the public IP is on an interface
	NATKindOneToOne    NATKind = "one_to_one"   // 1:1 NAT (cloud provider or container)
	NATKindPortForward NATKind = "port_forward" // a home router forwarding ports
	NATKindSymmetric   NATKind = "symmetric"    // mapped ports differ per destination; friends probably can't connect
	NATKindCGNATLikely NATKind = "cgnat_likely" // a 100.64.0.0/10 address on a non-Tailscale interface
	NATKindUnknown     NATKind = "unknown"
)

// NATKinds returns every NATKind value in the order of 04 §7.4.
func NATKinds() []NATKind {
	return []NATKind{
		NATKindNone, NATKindOneToOne, NATKindPortForward, NATKindSymmetric, NATKindCGNATLikely, NATKindUnknown,
	}
}

// ContainerKind is the container runtime the server runs in (ConnTestServerInfo.Container, DoctorEnv.Container).
type ContainerKind string

const (
	ContainerKindNone   ContainerKind = "none"
	ContainerKindDocker ContainerKind = "docker"
	ContainerKindPodman ContainerKind = "podman"
	ContainerKindOther  ContainerKind = "other"
)

// ConnTestRequest is POST /api/v1/conntest (04 §7.7): a real ICE check on one transport.
type ConnTestRequest struct {
	Transport Transport `json:"transport"`
	Offer     string    `json:"offer"` // SDP with one data channel; fmt and slog show only its length (secret.go)
}

// ConnTestResponse is the reply to POST /api/v1/conntest. Errors: 400 bad_sdp, 409 transport_disabled
// (params.transport), 429 rate_limited, 503 not_ready.
type ConnTestResponse struct {
	Answer     string             `json:"answer"`     // complete, non-trickle SDP with only that transport's candidates
	ExpiresInS int                `json:"expiresInS"` // the probe PC closes after this many seconds (20)
	Server     ConnTestServerInfo `json:"server"`
}

// ConnTestServerInfo lets the SPA show provider-specific fix text (to admins only).
type ConnTestServerInfo struct {
	PublicIP  string        `json:"publicIp"` // "" when no public IPv4 is known
	Provider  CloudProvider `json:"provider"`
	Container ContainerKind `json:"container"`
	UDPPort   int           `json:"udpPort"`  // 0 when UDP is off
	TCPPorts  []int         `json:"tcpPorts"` // e.g. [443, 7882]
	NAT       NATKind       `json:"nat"`
}
