package channels

import (
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"
)

const pushRequestTimeout = 10 * time.Second

// pushHTTPClient is used for every Web Push call. The endpoint is a
// user-supplied URL, so the client refuses non-public destinations at dial
// time (which also covers DNS rebinding and redirects), never follows
// redirects, and bounds the whole request so one slow push service cannot
// stall the delivery workers.
var pushHTTPClient = &http.Client{
	Timeout: pushRequestTimeout,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
			Control: refuseNonPublicAddr,
		}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: pushRequestTimeout,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func refuseNonPublicAddr(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !isPublicIP(ip) {
		return errors.New("push: destination address is not public")
	}
	return nil
}

func isPublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast())
}
