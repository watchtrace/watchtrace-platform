package config

import (
	"crypto/ed25519"
	"crypto/tls"
	"fmt"
)

const (
	gatewayConfigEnvironment      = "WATCHTRACE_GATEWAY_CONFIG"
	gatewaySigningKeyEnvironment  = "WATCHTRACE_GATEWAY_CONFIG_SIGNING_PUBLIC_KEY"
	gatewayLeaseKeyEnvironment    = "WATCHTRACE_GATEWAY_LEASE_KEY"
	gatewayCertificateEnvironment = "WATCHTRACE_GATEWAY_CERT"
	gatewayPrivateKeyEnvironment  = "WATCHTRACE_GATEWAY_KEY"
	gatewayClientCAEnvironment    = "WATCHTRACE_GATEWAY_CLIENT_CA"
	gatewayAddressEnvironment     = "WATCHTRACE_GATEWAY_ADDRESS"
	gatewaySQSEndpointEnvironment = "WATCHTRACE_SQS_ENDPOINT"
	defaultGatewayAddress         = "127.0.0.1:8443"
)

type QueueGatewayConfig struct {
	SignedConfig     []byte
	SigningPublicKey ed25519.PublicKey
	LeaseKey         []byte
	ServerTLS        *tls.Config
	Address          string
	SQSEndpoint      string
}

func LoadQueueGateway() (QueueGatewayConfig, error) {
	return loadQueueGateway(systemCommandSource())
}

func loadQueueGateway(source commandSource) (QueueGatewayConfig, error) {
	configPath, err := source.required(gatewayConfigEnvironment)
	if err != nil {
		return QueueGatewayConfig{}, err
	}
	signedConfig, err := source.readFile(configPath)
	if err != nil {
		return QueueGatewayConfig{}, fmt.Errorf("%s could not be read", gatewayConfigEnvironment)
	}
	signingKey, err := source.base64KeyFile(gatewaySigningKeyEnvironment, ed25519.PublicKeySize)
	if err != nil {
		return QueueGatewayConfig{}, err
	}
	leaseKey, err := source.base64KeyFile(gatewayLeaseKeyEnvironment, 32)
	if err != nil {
		return QueueGatewayConfig{}, err
	}
	serverTLS, err := source.serverMTLS(gatewayCertificateEnvironment, gatewayPrivateKeyEnvironment, gatewayClientCAEnvironment)
	if err != nil {
		return QueueGatewayConfig{}, err
	}
	address := source.value(gatewayAddressEnvironment, defaultGatewayAddress)
	if err = validateListenAddress(gatewayAddressEnvironment, address); err != nil {
		return QueueGatewayConfig{}, err
	}
	return QueueGatewayConfig{
		SignedConfig: signedConfig, SigningPublicKey: ed25519.PublicKey(signingKey), LeaseKey: leaseKey,
		ServerTLS: serverTLS, Address: address, SQSEndpoint: source.optional(gatewaySQSEndpointEnvironment),
	}, nil
}
