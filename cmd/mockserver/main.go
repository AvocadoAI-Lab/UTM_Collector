// mockserver 為開發與整合測試用的事件接收端（HTTPS，自簽憑證）。
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pico-utm-agent/internal/mockserver"
)

func main() {
	addr := flag.String("addr", ":8443", "監聽位址")
	token := flag.String("token", "dev-token", "Bearer token")
	minVersion := flag.String("min-version", "1.0.0", "心跳回應的 min_supported_version")
	certDir := flag.String("cert-dir", "mock-certs", "自簽憑證存放目錄（不存在時自動產生）")
	hosts := flag.String("hosts", "localhost,127.0.0.1", "憑證包含的主機名稱或 IP（逗號分隔）")
	flag.Parse()

	certFile, keyFile, err := ensureCert(*certDir, strings.Split(*hosts, ","))
	if err != nil {
		log.Fatalf("產生憑證失敗：%v", err)
	}
	srv := mockserver.New(*token)
	srv.SetMinSupportedVersion(*minVersion)

	log.Printf("mock server 監聽 https://%s", *addr)
	log.Printf("agent 設定：forwarder.ca_file = %q", certFile)
	log.Printf("控制端點：GET /_control/stats、GET /_control/events、POST|DELETE /_control/fault、POST /_control/min-version、POST /_control/reset")
	if err := http.ListenAndServeTLS(*addr, certFile, keyFile, logRequests(srv)); err != nil {
		log.Fatal(err)
	}
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: 0}
		defer func() {
			status := "斷線"
			if rec.status != 0 {
				status = http.StatusText(rec.status)
			}
			log.Printf("%s %s → %d %s (%s)", r.Method, r.URL.Path, rec.status, status, time.Since(start).Round(time.Millisecond))
		}()
		h.ServeHTTP(rec, r)
	})
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ensureCert 產生自簽憑證（本身即為 CA），agent 以 ca_file 信任它。
func ensureCert(dir string, hosts []string) (string, string, error) {
	certFile, keyFile := filepath.Join(dir, "mock-ca.pem"), filepath.Join(dir, "mock-key.pem")
	if _, err := os.Stat(certFile); err == nil {
		if _, err := os.Stat(keyFile); err == nil {
			return certFile, keyFile, nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "pico-utm mock collector"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(2, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}
