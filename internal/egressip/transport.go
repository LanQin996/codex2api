package egressip

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	proxyDialTimeout = 5 * time.Second
	// socks5Version 是 RFC 1928 的协议版本号。
	socks5Version = 0x05
)

// newProxyTransport 构造一条只经由 proxyURL 出网的传输：HTTP/HTTPS 代理用标准库的
// 代理支持，SOCKS5 用本文件里的最小实现（标准库没有 SOCKS5 客户端）。除这四种协议外
// 一律拒绝，绝不静默改成直连。
func newProxyTransport(proxy *url.URL) (*http.Transport, error) {
	transport := &http.Transport{
		DisableKeepAlives: true,
		// 出口 IP 查询是一次性请求，不需要 HTTP/2 与长连接。
		ForceAttemptHTTP2: false,
	}
	switch strings.ToLower(proxy.Scheme) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(proxy)
	case "socks5", "socks5h":
		dialer := &socks5Dialer{address: proxy.Host, timeout: proxyDialTimeout}
		if proxy.User != nil {
			dialer.username = proxy.User.Username()
			dialer.password, _ = proxy.User.Password()
		}
		transport.Proxy = nil
		transport.DialContext = dialer.DialContext
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", proxy.Scheme)
	}
	return transport, nil
}

// socks5Dialer 是只支持 CONNECT 的最小 SOCKS5 客户端（RFC 1928，凭据走 RFC 1929）。
// 目标主机名原样交给代理解析（等同 socks5h），本机不做 DNS。
type socks5Dialer struct {
	address  string
	username string
	password string
	timeout  time.Duration
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, errors.New("socks5: invalid target address")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, errors.New("socks5: invalid target port")
	}
	dialer := &net.Dialer{Timeout: d.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", d.address)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(d.timeout))
	}
	if err := d.handshake(conn, host, uint16(portNumber)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func (d *socks5Dialer) handshake(conn net.Conn, host string, port uint16) error {
	methods := []byte{0x00}
	if d.username != "" || d.password != "" {
		methods = append(methods, 0x02)
	}
	if _, err := conn.Write(append([]byte{socks5Version, byte(len(methods))}, methods...)); err != nil {
		return errors.New("socks5: greeting failed")
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		return errors.New("socks5: greeting reply failed")
	}
	if greeting[0] != socks5Version {
		return errors.New("socks5: proxy replied with an unexpected version")
	}
	switch greeting[1] {
	case 0x00:
	case 0x02:
		if err := d.authenticate(conn); err != nil {
			return err
		}
	default:
		return errors.New("socks5: proxy rejected all offered authentication methods")
	}

	request := []byte{socks5Version, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			request = append(request, 0x01)
			request = append(request, v4...)
		} else {
			request = append(request, 0x04)
			request = append(request, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("socks5: target host name too long")
		}
		request = append(request, 0x03, byte(len(host)))
		request = append(request, host...)
	}
	request = binary.BigEndian.AppendUint16(request, port)
	if _, err := conn.Write(request); err != nil {
		return errors.New("socks5: connect request failed")
	}

	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return errors.New("socks5: connect reply failed")
	}
	if reply[0] != socks5Version {
		return errors.New("socks5: proxy replied with an unexpected version")
	}
	if reply[1] != 0x00 {
		return fmt.Errorf("socks5: proxy refused the connection (code %d)", reply[1])
	}
	// 隧道已建立，BND.ADDR/BND.PORT 只是代理的本地地址，读完丢弃即可。
	var err error
	switch reply[3] {
	case 0x01:
		_, err = io.CopyN(io.Discard, conn, 4)
	case 0x04:
		_, err = io.CopyN(io.Discard, conn, 16)
	case 0x03:
		length := make([]byte, 1)
		if _, err = io.ReadFull(conn, length); err == nil {
			_, err = io.CopyN(io.Discard, conn, int64(length[0]))
		}
	default:
		return errors.New("socks5: proxy replied with an unexpected address type")
	}
	if err != nil {
		return errors.New("socks5: connect reply truncated")
	}
	if _, err := io.CopyN(io.Discard, conn, 2); err != nil {
		return errors.New("socks5: connect reply truncated")
	}
	return nil
}

func (d *socks5Dialer) authenticate(conn net.Conn) error {
	if len(d.username) > 255 || len(d.password) > 255 {
		return errors.New("socks5: credentials too long")
	}
	request := []byte{0x01, byte(len(d.username))}
	request = append(request, d.username...)
	request = append(request, byte(len(d.password)))
	request = append(request, d.password...)
	if _, err := conn.Write(request); err != nil {
		return errors.New("socks5: authentication request failed")
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return errors.New("socks5: authentication reply failed")
	}
	if reply[1] != 0x00 {
		return errors.New("socks5: proxy rejected the credentials")
	}
	return nil
}
