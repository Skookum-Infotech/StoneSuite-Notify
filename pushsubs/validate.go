package pushsubs

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

const (
	maxEndpointLen = 2048
	maxKeyLen      = 256
)

// Validate rejects subscriptions that could turn a push send into a request to
// an arbitrary internal address: the endpoint is called server-side with our
// VAPID credentials. Browsers only ever hand out https push-service URLs with
// a DNS name, so anything else is not a real subscription.
func Validate(endpoint, p256dh, auth string) error {
	if len(endpoint) > maxEndpointLen || len(p256dh) > maxKeyLen || len(auth) > maxKeyLen {
		return errors.New("subscription field too long")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("endpoint is not a valid URL")
	}
	if u.Scheme != "https" {
		return errors.New("endpoint must use https")
	}
	if u.User != nil {
		return errors.New("endpoint must not contain credentials")
	}
	host := u.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return errors.New("endpoint must use a host name")
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") || strings.HasSuffix(lower, ".internal") || !strings.Contains(lower, ".") {
		return errors.New("endpoint host is not allowed")
	}
	return nil
}
