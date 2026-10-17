package frontend

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	errInvalidHost   = errors.New("Host 不是本机地址")
	errInvalidOrigin = errors.New("Origin 不受信任")
	errOffLink       = errors.New("仅允许本机或同一链路的客户端")
	errUnauthorized  = errors.New("缺少或错误的 bearer token")
)

type networkState struct {
	ips      []net.IP
	networks []*net.IPNet
	at       time.Time
}

type accessPolicy struct {
	allowLAN       bool
	allowedOrigins map[string]struct{}
	// requireToken 要求非环回来源携带 token。
	requireToken bool
	// loopbackAnonymous 允许环回来源跳过 token 校验。
	loopbackAnonymous bool
	tokens            *tokenStore
	mu                sync.Mutex
	cached            networkState
	now               func() time.Time
	interfaces        func() ([]net.Interface, error)
}

func newAccessPolicy(listenAddr string, origins []string, security SecurityPolicy) *accessPolicy {
	allowLAN := true
	if ip := net.ParseIP(strings.Trim(strings.TrimSpace(listenAddr), "[]")); ip != nil && ip.IsLoopback() {
		allowLAN = false
	}
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		origin = strings.TrimSpace(origin)
		if origin != "" {
			allowed[origin] = struct{}{}
		}
	}
	return &accessPolicy{
		allowLAN:          allowLAN,
		allowedOrigins:    allowed,
		requireToken:      security.RequireToken,
		loopbackAnonymous: security.LoopbackAnonymous,
		tokens:            security.Tokens,
		now:               time.Now,
		interfaces:        net.Interfaces,
	}
}

// SecurityPolicy 把认证相关的运行时配置打包传给 accessPolicy。
type SecurityPolicy struct {
	RequireToken      bool
	LoopbackAnonymous bool
	Tokens            *tokenStore
}

func (p *accessPolicy) validate(r *http.Request) error {
	remoteIP := remoteAddressIP(r.RemoteAddr)
	if remoteIP == nil {
		return errOffLink
	}
	state, err := p.networks()
	if err != nil {
		return fmt.Errorf("读取本机网络接口: %w", err)
	}
	loopback := remoteIP.IsLoopback()
	if !loopback {
		if !p.allowLAN || !isDirectlyOnLink(remoteIP, state.networks) {
			return errOffLink
		}
	}
	if !validRequestHost(r.Host, remoteIP, state.ips) {
		return errInvalidHost
	}
	if err := p.validateOrigin(r); err != nil {
		return err
	}
	// 身份校验放在链路与来源校验之后：先确认「是不是本网段的可信来源」，
	// 再判断「有没有凭证」。这样外人连 401 都拿不到，减少信息暴露。
	if err := p.authorize(r, loopback); err != nil {
		return err
	}
	return nil
}

// authorize 按配置决定是否需要 bearer token。
func (p *accessPolicy) authorize(r *http.Request, loopback bool) error {
	if loopback && p.loopbackAnonymous {
		return nil
	}
	// 探活与版本查询始终免认证：否则外部监控、systemd/管理器健康检查
	// 在认证开启后会全部失效，反而掩盖真实故障。
	// 这两个端点不返回任何敏感信息（仅存活状态与版本号）。
	if authExemptPaths[r.URL.Path] {
		return nil
	}
	if !p.requireToken {
		return nil
	}
	if p.tokens == nil {
		// 配置要求认证但 token 存储未就绪：拒绝而不是放行，
		// 否则「需要认证」会退化成裸奔。
		return errTokenMissing
	}
	presented := bearerFromRequest(r)
	if presented == "" {
		return errUnauthorized
	}
	if !p.tokens.verify(presented) {
		return errUnauthorized
	}
	return nil
}

// authExemptPaths 是免认证路径。刻意保持最小：只放不影响安全判断的探针。
var authExemptPaths = map[string]bool{
	"/health":  true,
	"/version": true,
}

func (p *accessPolicy) validateOrigin(r *http.Request) error {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return nil
	}
	if _, ok := p.allowedOrigins[origin]; ok {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errInvalidOrigin
	}
	if !sameHostPort(u.Host, r.Host, u.Scheme, requestScheme(r)) {
		return errInvalidOrigin
	}
	return nil
}

func (p *accessPolicy) networks() (networkState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if !p.cached.at.IsZero() && now.Sub(p.cached.at) < 5*time.Second {
		return p.cached, nil
	}
	ifaces, err := p.interfaces()
	if err != nil {
		return networkState{}, err
	}
	next := networkState{at: now}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagPointToPoint != 0 ||
			isCellularInterface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, network := addressNetwork(addr)
			if ip == nil || network == nil {
				continue
			}
			next.ips = append(next.ips, ip)
			next.networks = append(next.networks, network)
		}
	}
	next.ips = append(next.ips, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))
	p.cached = next
	return next, nil
}

func isCellularInterface(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{
		"rmnet", "ccmni", "pdp", "wwan", "cellular", "v4-rmnet", "r_rmnet",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func addressNetwork(addr net.Addr) (net.IP, *net.IPNet) {
	switch value := addr.(type) {
	case *net.IPNet:
		return value.IP, value
	case *net.IPAddr:
		bits := 128
		if value.IP.To4() != nil {
			bits = 32
		}
		return value.IP, &net.IPNet{IP: value.IP, Mask: net.CIDRMask(bits, bits)}
	default:
		ip, network, err := net.ParseCIDR(addr.String())
		if err != nil {
			return nil, nil
		}
		return ip, network
	}
}

func remoteAddressIP(remote string) net.IP {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	if zone := strings.LastIndexByte(host, '%'); zone >= 0 {
		host = host[:zone]
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

func isDirectlyOnLink(remote net.IP, networks []*net.IPNet) bool {
	if remote.IsUnspecified() || remote.IsMulticast() {
		return false
	}
	for _, network := range networks {
		if network == nil || network.IP.IsLoopback() || network.IP.IsUnspecified() {
			continue
		}
		if network.Contains(remote) {
			return true
		}
	}
	return false
}

func validRequestHost(hostport string, remote net.IP, localIPs []net.IP) bool {
	host := hostOnly(hostport)
	if strings.EqualFold(host, "localhost") {
		return remote.IsLoopback()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return remote.IsLoopback()
	}
	for _, local := range localIPs {
		if local != nil && local.Equal(ip) {
			return true
		}
	}
	return false
}

func hostOnly(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err == nil {
		return strings.Trim(strings.SplitN(host, "%", 2)[0], "[]")
	}
	return strings.Trim(strings.SplitN(hostport, "%", 2)[0], "[]")
}

func sameHostPort(left, right, leftScheme, rightScheme string) bool {
	leftHost, leftPort := normalizedHostPort(left, leftScheme)
	rightHost, rightPort := normalizedHostPort(right, rightScheme)
	return strings.EqualFold(leftHost, rightHost) && leftPort == rightPort
}

func normalizedHostPort(value, scheme string) (string, string) {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		host = strings.Trim(value, "[]")
	}
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if zone := strings.LastIndexByte(host, '%'); zone >= 0 {
		host = host[:zone]
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	if parsed, err := strconv.Atoi(port); err == nil {
		port = strconv.Itoa(parsed)
	}
	return host, port
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
