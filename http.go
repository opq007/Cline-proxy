package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var execCommand = exec.Command

var httpTransport = &http.Transport{
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 10,
	IdleConnTimeout:     90 * time.Second,
	DisableCompression:  false,
}

var httpClient = &http.Client{
	Transport: httpTransport,
}

// proxyClientCache 按规范化 proxyURL 缓存 *http.Client，避免每次请求重建连接池。
var (
	proxyClientCache   = make(map[string]*http.Client)
	proxyClientCacheMu sync.RWMutex
)

// normalizeProxyURL 校验并规范化代理 URL。空字符串表示宿主机直连（合法）。
// 支持 scheme: http / https / socks5 / socks5h。
func normalizeProxyURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid proxy URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("unsupported proxy scheme %q (use http, https, socks5, socks5h)", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("proxy URL missing host")
	}
	u.Scheme = scheme
	return u.String(), nil
}

// clientForProxyURL 返回出站 HTTP 客户端。proxyURL 为空则使用全局直连 client。
func clientForProxyURL(proxyURL string) (*http.Client, error) {
	normalized, err := normalizeProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	if normalized == "" {
		return httpClient, nil
	}

	proxyClientCacheMu.RLock()
	if c, ok := proxyClientCache[normalized]; ok {
		proxyClientCacheMu.RUnlock()
		return c, nil
	}
	proxyClientCacheMu.RUnlock()

	transport, err := buildProxiedTransport(normalized)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: transport}

	proxyClientCacheMu.Lock()
	if existing, ok := proxyClientCache[normalized]; ok {
		proxyClientCacheMu.Unlock()
		return existing, nil
	}
	proxyClientCache[normalized] = client
	proxyClientCacheMu.Unlock()
	return client, nil
}

// clientForAccount 按账号 ProxyURL 取客户端；账号为 nil 或 ProxyURL 为空则直连。
func clientForAccount(acc *Account) (*http.Client, error) {
	if acc == nil {
		return httpClient, nil
	}
	return clientForProxyURL(acc.ProxyURL)
}

func buildProxiedTransport(normalizedProxyURL string) (*http.Transport, error) {
	u, err := url.Parse(normalizedProxyURL)
	if err != nil {
		return nil, err
	}

	t := httpTransport.Clone()
	scheme := strings.ToLower(u.Scheme)

	switch scheme {
	case "http", "https":
		t.Proxy = http.ProxyURL(u)
	case "socks5", "socks5h":
		var user, pass string
		if u.User != nil {
			user = u.User.Username()
			pass, _ = u.User.Password()
		}
		socksAddr := u.Host
		// socks5h：域名由代理解析；socks5：先本地解析再 CONNECT
		resolveLocal := scheme == "socks5"
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialSOCKS5(ctx, socksAddr, user, pass, addr, resolveLocal)
		}
		t.Proxy = nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", scheme)
	}
	return t, nil
}

// dialSOCKS5 纯标准库 SOCKS5 客户端（RFC 1928 + 可选用户名密码 RFC 1929）。
func dialSOCKS5(ctx context.Context, proxyAddr, user, pass, target string, resolveLocal bool) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("socks5 connect proxy: %w", err)
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	var methods []byte
	if user != "" {
		methods = []byte{0x00, 0x02}
	} else {
		methods = []byte{0x00}
	}
	greet := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greet); err != nil {
		return nil, fmt.Errorf("socks5 greeting: %w", err)
	}

	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, fmt.Errorf("socks5 greeting resp: %w", err)
	}
	if resp[0] != 0x05 {
		return nil, fmt.Errorf("socks5 bad version %d", resp[0])
	}

	switch resp[1] {
	case 0x00:
	case 0x02:
		if user == "" {
			return nil, fmt.Errorf("socks5 server requires username/password")
		}
		if len(user) > 255 || len(pass) > 255 {
			return nil, fmt.Errorf("socks5 auth credentials too long")
		}
		authMsg := []byte{0x01, byte(len(user))}
		authMsg = append(authMsg, []byte(user)...)
		authMsg = append(authMsg, byte(len(pass)))
		authMsg = append(authMsg, []byte(pass)...)
		if _, err := conn.Write(authMsg); err != nil {
			return nil, fmt.Errorf("socks5 auth: %w", err)
		}
		authResp := make([]byte, 2)
		if _, err := io.ReadFull(conn, authResp); err != nil {
			return nil, fmt.Errorf("socks5 auth resp: %w", err)
		}
		if authResp[1] != 0x00 {
			return nil, fmt.Errorf("socks5 auth failed (status %d)", authResp[1])
		}
	case 0xFF:
		return nil, fmt.Errorf("socks5 no acceptable auth method")
	default:
		return nil, fmt.Errorf("socks5 unsupported auth method %d", resp[1])
	}

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, fmt.Errorf("socks5 bad target %q: %w", target, err)
	}
	port, err := net.LookupPort("tcp", portStr)
	if err != nil {
		return nil, fmt.Errorf("socks5 bad port: %w", err)
	}

	var req []byte
	req = append(req, 0x05, 0x01, 0x00)

	if resolveLocal {
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("socks5 resolve %s: %w", host, err)
		}
		ip := ips[0].IP
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			ip16 := ip.To16()
			if ip16 == nil {
				return nil, fmt.Errorf("socks5 invalid ip for %s", host)
			}
			req = append(req, 0x04)
			req = append(req, ip16...)
		}
	} else {
		if len(host) > 255 {
			return nil, fmt.Errorf("socks5 domain too long")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, []byte(host)...)
	}

	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, uint16(port))
	req = append(req, portBuf...)

	if _, err := conn.Write(req); err != nil {
		return nil, fmt.Errorf("socks5 connect req: %w", err)
	}

	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return nil, fmt.Errorf("socks5 connect resp: %w", err)
	}
	if hdr[0] != 0x05 {
		return nil, fmt.Errorf("socks5 bad connect version")
	}
	if hdr[1] != 0x00 {
		return nil, fmt.Errorf("socks5 connect failed: rep=%d", hdr[1])
	}
	var skip int
	switch hdr[3] {
	case 0x01:
		skip = 4
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return nil, err
		}
		skip = int(l[0])
	case 0x04:
		skip = 16
	default:
		return nil, fmt.Errorf("socks5 unknown atyp %d", hdr[3])
	}
	rest := make([]byte, skip+2)
	if _, err := io.ReadFull(conn, rest); err != nil {
		return nil, fmt.Errorf("socks5 bind addr: %w", err)
	}

	_ = conn.SetDeadline(time.Time{})
	success = true
	return conn, nil
}

func invalidateProxyClient(proxyURL string) {
	normalized, err := normalizeProxyURL(proxyURL)
	if err != nil || normalized == "" {
		return
	}
	proxyClientCacheMu.Lock()
	delete(proxyClientCache, normalized)
	proxyClientCacheMu.Unlock()
}

func httpPostForm(rawURL string, form url.Values) (*http.Response, error) {
	return httpPostFormWithClient(httpClient, rawURL, form)
}

func httpPostFormWithClient(client *http.Client, rawURL string, form url.Values) (*http.Response, error) {
	if client == nil {
		client = httpClient
	}
	req, err := http.NewRequest("POST", rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return client.Do(req)
}

func httpPostJSON(rawURL string, body any) (*http.Response, error) {
	return httpPostJSONWithClient(httpClient, rawURL, body)
}

func httpPostJSONWithClient(client *http.Client, rawURL string, body any) (*http.Response, error) {
	if client == nil {
		client = httpClient
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", rawURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}

func readBody(resp *http.Response) string {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("<read error: %v>", err)
	}
	return string(data)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Start()
}
