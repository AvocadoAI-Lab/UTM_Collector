package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

type apiClient struct {
	endpoint string
	token    string
	proxy    func(*http.Request) (*url.URL, error)
	client   *http.Client
}

type apiResponse struct {
	status int
	header http.Header
	body   []byte
}

// newAPIClient 建立 HTTPS 用戶端：TLS 1.2 以上、一律驗證伺服器憑證。
// proxy 優先順序：設定檔 > HTTPS_PROXY / HTTP_PROXY / NO_PROXY 環境變數。
// rootCAs 僅供測試注入；正式環境使用系統憑證庫加上選用的 ca_file。
func newAPIClient(cfg *Config, rootCAs *x509.CertPool) (*apiClient, error) {
	if rootCAs == nil && cfg.Forwarder.CAFile != "" {
		pem, err := os.ReadFile(cfg.Forwarder.CAFile)
		if err != nil {
			return nil, fmt.Errorf("讀取 ca_file 失敗：%w", err)
		}
		rootCAs, err = x509.SystemCertPool()
		if err != nil || rootCAs == nil {
			rootCAs = x509.NewCertPool()
		}
		if !rootCAs.AppendCertsFromPEM(pem) {
			return nil, errors.New("ca_file 中沒有有效的 PEM 憑證")
		}
	}
	proxy := http.ProxyFromEnvironment
	if cfg.Proxy.URL != "" {
		u, err := url.Parse(cfg.Proxy.URL)
		if err != nil {
			return nil, errors.New("proxy.url 不合法")
		}
		proxy = http.ProxyURL(u)
	}
	tr := &http.Transport{
		Proxy:                 proxy,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: rootCAs},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &apiClient{
		endpoint: cfg.endpoint(),
		token:    cfg.Forwarder.Token,
		proxy:    proxy,
		client:   &http.Client{Transport: tr, Timeout: 60 * time.Second},
	}, nil
}

func (c *apiClient) post(ctx context.Context, path string, payload any) (*apiResponse, error) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	enc := json.NewEncoder(zw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, &gz)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	return c.do(req)
}

func (c *apiClient) get(ctx context.Context, path string) (*apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *apiClient) do(req *http.Request) (*apiResponse, error) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "pico-utm-agent/"+Version)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	return &apiResponse{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// proxyFor 回傳連線到端點時實際使用的 proxy；直連時為 nil。
func (c *apiClient) proxyFor() (*url.URL, error) {
	req, err := http.NewRequest(http.MethodGet, c.endpoint, nil)
	if err != nil {
		return nil, err
	}
	return c.proxy(req)
}
