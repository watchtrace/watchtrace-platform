package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadMonitorEngineMatrix(t *testing.T) {
	values, files := monitorEngineFixture()
	configuration, err := loadMonitorEngine(testCommandSource(values, files))
	if err != nil {
		t.Fatalf("loadMonitorEngine: %v", err)
	}
	if configuration.DatabaseURL != validDatabaseURL || configuration.SigningKeyID != "platform-v1" ||
		configuration.HealthAddress != defaultMonitorHealthAddress || len(configuration.SigningKey) != ed25519.PrivateKeySize {
		t.Fatalf("unexpected monitor-engine configuration: %+v", configuration)
	}

	tests := []struct {
		name, environment, value, want string
	}{
		{"missing queue", monitorJobQueueEnvironment, "", monitorJobQueueEnvironment + " is required"},
		{"bad health address", monitorHealthAddressEnvironment, "localhost", monitorHealthAddressEnvironment + " must be in host:port form"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caseValues := copyStrings(values)
			caseValues[test.environment] = test.value
			_, loadErr := loadMonitorEngine(testCommandSource(caseValues, files))
			if loadErr == nil || loadErr.Error() != test.want {
				t.Fatalf("error = %v, want %q", loadErr, test.want)
			}
		})
	}
}

func TestLoadNotificationWorkerMatrix(t *testing.T) {
	base := map[string]string{databaseURLEnvironment: validDatabaseURL}
	local, err := loadNotificationWorker(testCommandSource(base, nil))
	if err != nil {
		t.Fatalf("load local notification worker: %v", err)
	}
	if local.Provider != "local" || local.HealthAddress != defaultNotificationHealthAddress || local.WorkerID != "notification-worker-1" {
		t.Fatalf("unexpected local defaults: %+v", local)
	}

	ociValues := copyStrings(base)
	ociValues[notificationProviderEnvironment] = " OCI "
	ociValues[notificationSMTPAddressEnvironment] = "smtp.email.example.test:587"
	ociValues[notificationSMTPUsernameEnvironment] = "smtp-user"
	ociValues[notificationSMTPPasswordEnvironment] = "smtp-password"
	ociValues[notificationFromEnvironment] = "watchtrace@example.test"
	oci, err := loadNotificationWorker(testCommandSource(ociValues, nil))
	if err != nil || oci.Provider != "oci" {
		t.Fatalf("load OCI notification worker = %+v, %v", oci, err)
	}

	invalid := []struct {
		name   string
		values map[string]string
	}{
		{"unsupported provider", map[string]string{databaseURLEnvironment: validDatabaseURL, notificationProviderEnvironment: "ses"}},
		{"missing OCI credentials", map[string]string{databaseURLEnvironment: validDatabaseURL, notificationProviderEnvironment: "oci", notificationSMTPAddressEnvironment: "smtp.email.example.test:587", notificationFromEnvironment: "watchtrace@example.test"}},
		{"invalid health address", map[string]string{databaseURLEnvironment: validDatabaseURL, notificationHealthAddressEnvironment: "localhost"}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, loadErr := loadNotificationWorker(testCommandSource(test.values, nil)); loadErr == nil {
				t.Fatal("loadNotificationWorker succeeded, want an error")
			}
		})
	}
}

func TestLoadWorkerTransportMatrix(t *testing.T) {
	values, files := workerFixture()
	direct, err := loadWorker(testCommandSource(values, files))
	if err != nil {
		t.Fatalf("load direct-SQS worker: %v", err)
	}
	if direct.Transport != WorkerTransportDirectSQS || direct.ClientTLS != nil || direct.ClockOffset != 0 || len(direct.PrivateCIDRs) != 2 {
		t.Fatalf("unexpected direct-SQS configuration: %+v", direct)
	}

	certificate, privateKey := testCertificate(t)
	httpsValues := copyStrings(values)
	delete(httpsValues, workerJobQueueEnvironment)
	delete(httpsValues, workerResultQueueEnvironment)
	httpsValues[workerTransportEnvironment] = WorkerTransportHTTPS
	httpsValues[workerGatewayURLEnvironment] = "https://gateway.example.test/v1"
	httpsValues[workerMTLSCertificateEnvironment] = "/cert.pem"
	httpsValues[workerMTLSKeyEnvironment] = "/key.pem"
	httpsValues[workerGatewayCAEnvironment] = "/ca.pem"
	httpsFiles := copyBytes(files)
	httpsFiles["/cert.pem"] = certificate
	httpsFiles["/key.pem"] = privateKey
	httpsFiles["/ca.pem"] = certificate
	https, err := loadWorker(testCommandSource(httpsValues, httpsFiles))
	if err != nil || https.ClientTLS == nil || https.GatewayURL != "https://gateway.example.test/v1" {
		t.Fatalf("load HTTPS worker = %+v, %v", https, err)
	}

	invalid := []struct {
		name, environment, value string
	}{
		{"unsupported transport", workerTransportEnvironment, "grpc"},
		{"invalid CIDR", workerPrivateCIDRsEnvironment, "10.0.0.0/not-a-prefix"},
		{"excessive clock offset", workerClockOffsetEnvironment, "25h"},
		{"missing direct queue", workerResultQueueEnvironment, ""},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			caseValues := copyStrings(values)
			caseValues[test.environment] = test.value
			if _, loadErr := loadWorker(testCommandSource(caseValues, files)); loadErr == nil {
				t.Fatal("loadWorker succeeded, want an error")
			}
		})
	}
}

func TestLoadWorkerKeyring(t *testing.T) {
	values, files := workerFixture()
	values[workerKeyringEnvironment] = "/keyring.json"
	files["/rotated-worker"] = encodedKey(32)
	files["/rotated-platform"] = encodedKey(ed25519.PublicKeySize)
	files["/keyring.json"] = []byte(`{"worker_encryption":{"worker-v0":"/rotated-worker"},"platform_signing":{"platform-v0":"/rotated-platform"},"revoked":["retired-v0"]}`)

	configuration, err := loadWorker(testCommandSource(values, files))
	if err != nil {
		t.Fatalf("loadWorker: %v", err)
	}
	if len(configuration.Keyring.WorkerEncryption["worker-v0"]) != 32 ||
		len(configuration.Keyring.PlatformSigning["platform-v0"]) != ed25519.PublicKeySize {
		t.Fatalf("rotated keys were not loaded: %+v", configuration.Keyring)
	}
	if _, ok := configuration.Keyring.Revoked["retired-v0"]; !ok {
		t.Fatal("revoked key ID was not loaded")
	}
}

func TestLoadQueueGatewayMatrix(t *testing.T) {
	certificate, privateKey := testCertificate(t)
	values := map[string]string{
		gatewayConfigEnvironment:      "/gateway.json",
		gatewaySigningKeyEnvironment:  "/signing-public",
		gatewayLeaseKeyEnvironment:    "/lease-key",
		gatewayCertificateEnvironment: "/cert.pem",
		gatewayPrivateKeyEnvironment:  "/key.pem",
		gatewayClientCAEnvironment:    "/ca.pem",
	}
	files := map[string][]byte{
		"/gateway.json":   []byte(`{"payload":"signed"}`),
		"/signing-public": encodedKey(ed25519.PublicKeySize),
		"/lease-key":      encodedKey(32),
		"/cert.pem":       certificate,
		"/key.pem":        privateKey,
		"/ca.pem":         certificate,
	}
	configuration, err := loadQueueGateway(testCommandSource(values, files))
	if err != nil {
		t.Fatalf("loadQueueGateway: %v", err)
	}
	if configuration.Address != defaultGatewayAddress || configuration.ServerTLS == nil ||
		configuration.ServerTLS.ClientAuth != tls.RequireAndVerifyClientCert || len(configuration.SignedConfig) == 0 {
		t.Fatalf("unexpected queue-gateway configuration: %+v", configuration)
	}

	badAddress := copyStrings(values)
	badAddress[gatewayAddressEnvironment] = "localhost"
	if _, err = loadQueueGateway(testCommandSource(badAddress, files)); err == nil {
		t.Fatal("invalid gateway address was accepted")
	}
	missingConfig := copyBytes(files)
	delete(missingConfig, "/gateway.json")
	if _, err = loadQueueGateway(testCommandSource(values, missingConfig)); err == nil || strings.Contains(err.Error(), "/gateway.json") {
		t.Fatalf("unreadable config error was absent or exposed its path: %v", err)
	}
}

func TestCommandKeyErrorDoesNotExposeContentOrPath(t *testing.T) {
	const secretPath = "/run/secrets/do-not-expose-this-path"
	source := testCommandSource(map[string]string{"SECRET_FILE": secretPath}, map[string][]byte{secretPath: []byte("secret-content")})
	_, err := source.base64KeyFile("SECRET_FILE", 32)
	if err == nil {
		t.Fatal("invalid key succeeded")
	}
	if strings.Contains(err.Error(), secretPath) || strings.Contains(err.Error(), "secret-content") {
		t.Fatalf("configuration error exposed key material: %v", err)
	}
}

func monitorEngineFixture() (map[string]string, map[string][]byte) {
	values := map[string]string{
		databaseURLEnvironment:          validDatabaseURL,
		monitorSigningKeyEnvironment:    "/signing-private",
		engineHeaderKeyFileEnvironment:  "/header-key",
		monitorQuarantineKeyEnvironment: "/quarantine-key",
		monitorJobQueueEnvironment:      "https://sqs.example.test/jobs.fifo",
		monitorResultQueueEnvironment:   "https://sqs.example.test/results.fifo",
		monitorJobDLQEnvironment:        "https://sqs.example.test/jobs-dlq.fifo",
		monitorResultDLQEnvironment:     "https://sqs.example.test/results-dlq.fifo",
	}
	files := map[string][]byte{
		"/signing-private": encodedKey(ed25519.PrivateKeySize),
		"/header-key":      encodedKey(32),
		"/quarantine-key":  encodedKey(32),
	}
	return values, files
}

func workerFixture() (map[string]string, map[string][]byte) {
	values := map[string]string{
		workerPoolIDEnvironment:          "pool-1",
		workerIDEnvironment:              "worker-1",
		workerEncryptionKeyEnvironment:   "/worker-private",
		workerEncryptionKeyIDEnvironment: "worker-v1",
		workerResultKeyEnvironment:       "/result-private",
		workerResultKeyIDEnvironment:     "result-v1",
		workerPlatformKeyEnvironment:     "/platform-public",
		workerPlatformKeyIDEnvironment:   "platform-v1",
		workerTransportEnvironment:       WorkerTransportDirectSQS,
		workerJobQueueEnvironment:        "https://sqs.example.test/jobs.fifo",
		workerResultQueueEnvironment:     "https://sqs.example.test/results.fifo",
		workerPrivateCIDRsEnvironment:    "10.0.0.0/8,192.168.0.0/16",
	}
	files := map[string][]byte{
		"/worker-private":  encodedKey(32),
		"/result-private":  encodedKey(ed25519.PrivateKeySize),
		"/platform-public": encodedKey(ed25519.PublicKeySize),
	}
	return values, files
}

func testCommandSource(values map[string]string, files map[string][]byte) commandSource {
	return commandSource{
		lookup: environment(values),
		readFile: func(path string) ([]byte, error) {
			value, ok := files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return value, nil
		},
	}
}

func testCertificate(t *testing.T) ([]byte, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "watchtrace-test"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
}

func encodedKey(size int) []byte {
	return []byte(base64.StdEncoding.EncodeToString(make([]byte, size)))
}

func copyStrings(source map[string]string) map[string]string {
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func copyBytes(source map[string][]byte) map[string][]byte {
	copy := make(map[string][]byte, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}
