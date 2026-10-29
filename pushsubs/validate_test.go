package pushsubs

import "testing"

func TestValidate(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		ok       bool
	}{
		{"fcm", "https://fcm.googleapis.com/fcm/send/abc", true},
		{"http", "http://fcm.googleapis.com/x", false},
		{"ip literal", "https://169.254.169.254/latest", false},
		{"ipv6 loopback", "https://[::1]/x", false},
		{"localhost", "https://localhost/x", false},
		{"fly internal", "https://backend.internal/x", false},
		{"userinfo", "https://user:pw@fcm.googleapis.com/x", false},
		{"no dot host", "https://intranet/x", false},
		{"too long", "https://a.example.com/" + string(make([]byte, 3000)), false},
	}
	for _, c := range cases {
		if err := Validate(c.endpoint, "k", "a"); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}
