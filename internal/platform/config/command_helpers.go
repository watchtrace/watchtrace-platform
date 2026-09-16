package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type environmentLookup func(string) (string, bool)
type fileReader func(string) ([]byte, error)

type commandSource struct {
	lookup   environmentLookup
	readFile fileReader
}

func systemCommandSource() commandSource {
	return commandSource{lookup: os.LookupEnv, readFile: os.ReadFile}
}

func (source commandSource) value(name, fallback string) string {
	if value, exists := source.lookup(name); exists {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return fallback
}

func (source commandSource) optional(name string) string {
	value, _ := source.lookup(name)
	return strings.TrimSpace(value)
}

func (source commandSource) required(name string) (string, error) {
	value := source.optional(name)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func (source commandSource) base64KeyFile(environmentName string, sizes ...int) ([]byte, error) {
	path, err := source.required(environmentName)
	if err != nil {
		return nil, err
	}
	return source.base64KeyPath(path, environmentName, sizes...)
}

func (source commandSource) base64KeyPath(path, label string, sizes ...int) ([]byte, error) {
	data, err := source.readFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s could not be read", label)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || !allowedLength(len(decoded), sizes) {
		return nil, fmt.Errorf("%s contains an invalid base64 key", label)
	}
	return decoded, nil
}

func allowedLength(length int, allowed []int) bool {
	for _, candidate := range allowed {
		if length == candidate {
			return true
		}
	}
	return false
}

func validateListenAddress(name, value string) error {
	host, portValue, err := net.SplitHostPort(value)
	if err != nil || strings.ContainsAny(host, " \t\r\n") {
		return fmt.Errorf("%s must be in host:port form", name)
	}
	port, err := strconv.Atoi(portValue)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%s contains an invalid port", name)
	}
	return nil
}

func validateHTTPSURL(name, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must be an absolute HTTPS URL without credentials, query, or fragment", name)
	}
	return nil
}

func parseDuration(name, value string, maximum time.Duration) (time.Duration, error) {
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < -maximum || parsed > maximum {
		return 0, fmt.Errorf("%s must be a duration between %s and %s", name, -maximum, maximum)
	}
	return parsed, nil
}

func (source commandSource) clientTLS(certEnvironment, keyEnvironment, caEnvironment string) (*tls.Config, error) {
	certificatePath, err := source.required(certEnvironment)
	if err != nil {
		return nil, err
	}
	keyPath, err := source.required(keyEnvironment)
	if err != nil {
		return nil, err
	}
	caPath, err := source.required(caEnvironment)
	if err != nil {
		return nil, err
	}
	certificate, err := source.keyPair(certificatePath, keyPath, certEnvironment, keyEnvironment)
	if err != nil {
		return nil, err
	}
	roots, err := source.certificatePool(caPath, caEnvironment)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}, RootCAs: roots, MinVersion: tls.VersionTLS12}, nil
}

func (source commandSource) serverMTLS(certEnvironment, keyEnvironment, caEnvironment string) (*tls.Config, error) {
	certificatePath, err := source.required(certEnvironment)
	if err != nil {
		return nil, err
	}
	keyPath, err := source.required(keyEnvironment)
	if err != nil {
		return nil, err
	}
	caPath, err := source.required(caEnvironment)
	if err != nil {
		return nil, err
	}
	certificate, err := source.keyPair(certificatePath, keyPath, certEnvironment, keyEnvironment)
	if err != nil {
		return nil, err
	}
	roots, err := source.certificatePool(caPath, caEnvironment)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs: roots, MinVersion: tls.VersionTLS12,
	}, nil
}

func (source commandSource) keyPair(certificatePath, keyPath, certificateLabel, keyLabel string) (tls.Certificate, error) {
	certificate, err := source.readFile(certificatePath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s could not be read", certificateLabel)
	}
	key, err := source.readFile(keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("%s could not be read", keyLabel)
	}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		return tls.Certificate{}, errors.New("TLS certificate and key are invalid")
	}
	return pair, nil
}

func (source commandSource) certificatePool(path, label string) (*x509.CertPool, error) {
	data, err := source.readFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s could not be read", label)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("%s does not contain a valid CA certificate", label)
	}
	return pool, nil
}
