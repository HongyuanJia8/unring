package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type receivedRequest struct {
	Time           string `json:"time"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	Body           string `json:"body"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

func main() {
	runtimeDir := flag.String("runtime", "runtime", "directory for generated certificates and request log")
	port := flag.Int("port", 58443, "loopback HTTPS port")
	flag.Parse()
	if err := os.MkdirAll(*runtimeDir, 0o700); err != nil {
		log.Fatal(err)
	}
	caPath := filepath.Join(*runtimeDir, "fake-ca.pem")
	certPath := filepath.Join(*runtimeDir, "fake-service.pem")
	keyPath := filepath.Join(*runtimeDir, "fake-service-key.pem")
	if err := ensureCertificates(caPath, certPath, keyPath); err != nil {
		log.Fatal(err)
	}

	receivedPath := filepath.Join(*runtimeDir, "received.ndjson")
	var receivedMu sync.Mutex
	record := func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		item := receivedRequest{
			Time: time.Now().UTC().Format(time.RFC3339Nano), Method: request.Method,
			Path: request.URL.RequestURI(), Body: string(body),
			IdempotencyKey: request.Header.Get("Idempotency-Key"),
		}
		line, _ := json.Marshal(item)
		receivedMu.Lock()
		file, openErr := os.OpenFile(receivedPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if openErr == nil {
			_, openErr = fmt.Fprintf(file, "%s\n", line)
			_ = file.Close()
		}
		receivedMu.Unlock()
		if openErr != nil {
			http.Error(response, openErr.Error(), http.StatusInternalServerError)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/approval" {
			response.WriteHeader(http.StatusCreated)
		} else {
			response.WriteHeader(http.StatusAccepted)
		}
		_, _ = io.WriteString(response, `{"received":true}`)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, "ok\n")
	})
	mux.HandleFunc("/stage", record)
	mux.HandleFunc("/approval", record)
	server := &http.Server{Addr: fmt.Sprintf("localhost:%d", *port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("fake HTTPS service listening on %s", server.Addr)
	log.Fatal(server.ListenAndServeTLS(certPath, keyPath))
}

func ensureCertificates(caPath, certPath, keyPath string) error {
	if _, err := os.Stat(certPath); err == nil {
		return nil
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unring demo CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return err
	}
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: leafKeyDER}), 0o600)
}
