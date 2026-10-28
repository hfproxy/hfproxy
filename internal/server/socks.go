package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func dialConfiguredProxy(ctx context.Context, network string, dialer *net.Dialer, proxy *url.URL) (net.Conn, error) {
	host := proxy.Hostname()
	port := proxy.Port()
	if port == "" {
		switch strings.ToLower(proxy.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			port = "1080"
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, candidate := range addresses {
		ip, ok := netip.AddrFromSlice(candidate.IP)
		if !ok || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("configured proxy resolved to no usable IP addresses")
	}
	return nil, lastErr
}

func dialSOCKS(ctx context.Context, network, targetAddress string, dialer *net.Dialer, proxy *url.URL, allowLoopback bool) (net.Conn, error) {
	host, port, err := net.SplitHostPort(targetAddress)
	if err != nil {
		return nil, err
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("invalid SOCKS destination port")
	}
	if ip, parseErr := netip.ParseAddr(host); parseErr == nil && !egressIPAllowed(ip, allowLoopback) {
		return nil, fmt.Errorf("egress to non-public IP is blocked")
	}

	conn, err := dialConfiguredProxy(ctx, network, dialer, proxy)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = conn.Close()
		}
	}()

	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	deadline := time.Now().Add(10 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	switch strings.ToLower(proxy.Scheme) {
	case "socks4", "socks4a":
		err = negotiateSOCKS4(ctx, conn, proxy, host, uint16(portNumber), allowLoopback, strings.EqualFold(proxy.Scheme, "socks4a"))
	case "socks5", "socks5h":
		err = negotiateSOCKS5(conn, proxy, host, uint16(portNumber))
	default:
		err = fmt.Errorf("unsupported SOCKS proxy scheme")
	}
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	keep = true
	return conn, nil
}

func negotiateSOCKS5(conn net.Conn, proxy *url.URL, targetHost string, targetPort uint16) error {
	if proxy.User != nil && proxy.User.Username() == "" {
		return fmt.Errorf("SOCKS5 proxy username must not be empty")
	}
	methods := []byte{0x00}
	if proxy.User != nil {
		methods = append(methods, 0x02)
	}
	greeting := []byte{0x05, byte(len(methods))}
	greeting = append(greeting, methods...)
	if err := writeAll(conn, greeting); err != nil {
		return fmt.Errorf("SOCKS5 greeting failed")
	}
	var selection [2]byte
	if _, err := io.ReadFull(conn, selection[:]); err != nil || selection[0] != 0x05 {
		return fmt.Errorf("invalid SOCKS5 greeting response")
	}
	switch selection[1] {
	case 0x00:
	case 0x02:
		if proxy.User == nil {
			return fmt.Errorf("SOCKS5 proxy requires credentials")
		}
		username := proxy.User.Username()
		password, _ := proxy.User.Password()
		auth := []byte{0x01, byte(len(username))}
		auth = append(auth, username...)
		auth = append(auth, byte(len(password)))
		auth = append(auth, password...)
		if err := writeAll(conn, auth); err != nil {
			return fmt.Errorf("SOCKS5 authentication failed")
		}
		var result [2]byte
		if _, err := io.ReadFull(conn, result[:]); err != nil || result[0] != 0x01 || result[1] != 0x00 {
			return fmt.Errorf("SOCKS5 proxy rejected its credentials")
		}
	default:
		return fmt.Errorf("SOCKS5 proxy selected an unsupported authentication method")
	}

	request := []byte{0x05, 0x01, 0x00}
	if ip, err := netip.ParseAddr(targetHost); err == nil {
		ip = ip.Unmap()
		if ip.Is4() {
			address := ip.As4()
			request = append(request, 0x01)
			request = append(request, address[:]...)
		} else {
			address := ip.As16()
			request = append(request, 0x04)
			request = append(request, address[:]...)
		}
	} else {
		name := []byte(targetHost)
		if len(name) == 0 || len(name) > 255 {
			return fmt.Errorf("SOCKS5 destination hostname is invalid")
		}
		request = append(request, 0x03, byte(len(name)))
		request = append(request, name...)
	}
	request = append(request, byte(targetPort>>8), byte(targetPort))
	if err := writeAll(conn, request); err != nil {
		return fmt.Errorf("SOCKS5 connect request failed")
	}
	var response [4]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil || response[0] != 0x05 {
		return fmt.Errorf("invalid SOCKS5 connect response")
	}
	if response[1] != 0x00 {
		return fmt.Errorf("SOCKS5 proxy could not connect to the Hugging Face host")
	}
	var addressLength int
	switch response[3] {
	case 0x01:
		addressLength = 4
	case 0x04:
		addressLength = 16
	case 0x03:
		var size [1]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return fmt.Errorf("invalid SOCKS5 bound address")
		}
		addressLength = int(size[0])
	default:
		return fmt.Errorf("invalid SOCKS5 bound address type")
	}
	if _, err := io.CopyN(io.Discard, conn, int64(addressLength+2)); err != nil {
		return fmt.Errorf("incomplete SOCKS5 connect response")
	}
	return nil
}

func negotiateSOCKS4(ctx context.Context, conn net.Conn, proxy *url.URL, targetHost string, targetPort uint16, allowLoopback, socks4a bool) error {
	var address [4]byte
	useDomain := false
	if ip, err := netip.ParseAddr(targetHost); err == nil {
		ip = ip.Unmap()
		if !ip.Is4() {
			return fmt.Errorf("SOCKS4 does not support IPv6 destinations")
		}
		ip4 := ip.As4()
		copy(address[:], ip4[:])
	} else if socks4a {
		address = [4]byte{0, 0, 0, 1}
		useDomain = true
	} else {
		ip, err := resolveSOCKS4Host(ctx, targetHost, allowLoopback)
		if err != nil {
			return err
		}
		ip4 := ip.As4()
		copy(address[:], ip4[:])
	}

	username := ""
	if proxy.User != nil {
		username = proxy.User.Username()
	}
	request := []byte{0x04, 0x01, byte(targetPort >> 8), byte(targetPort)}
	request = append(request, address[:]...)
	request = append(request, username...)
	request = append(request, 0x00)
	if useDomain {
		request = append(request, targetHost...)
		request = append(request, 0x00)
	}
	if err := writeAll(conn, request); err != nil {
		return fmt.Errorf("SOCKS4 connect request failed")
	}
	var response [8]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil || response[0] != 0x00 {
		return fmt.Errorf("invalid SOCKS4 connect response")
	}
	if response[1] != 0x5a {
		return fmt.Errorf("SOCKS4 proxy could not connect to the Hugging Face host")
	}
	return nil
}

func resolveSOCKS4Host(ctx context.Context, host string, allowLoopback bool) (netip.Addr, error) {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, candidate := range addresses {
		ip, ok := netip.AddrFromSlice(candidate.IP)
		if !ok || !ip.Unmap().Is4() {
			continue
		}
		ip = ip.Unmap()
		if egressIPAllowed(ip, allowLoopback) {
			return ip, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("SOCKS4 destination did not resolve to an allowed IPv4 address")
}

func egressIPAllowed(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.Unmap()
	if ip.Is6() && ip.As16()[0] == 0x20 && ip.As16()[1] == 0x02 {
		// 6to4 embeds an IPv4 destination in bytes 2..5; apply the IPv4
		// policy to the embedded address instead of trusting the IPv6 wrapper.
		b := ip.As16()
		return egressIPAllowed(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), allowLoopback)
	}
	for _, blocked := range []string{"100.64.0.0/10", "198.18.0.0/15", "64:ff9b::/96"} {
		if prefix, err := netip.ParsePrefix(blocked); err == nil && prefix.Contains(ip) {
			return false
		}
	}
	allowed := ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLoopback() && !ip.IsUnspecified()
	return allowed || (allowLoopback && ip.IsLoopback())
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}
