package netx

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marcus-beauregard/agent-runner/internal/report"
)

// TestAllows pins the matching rules, including the one that matters: a name
// that merely ends in an allowlisted host is not the allowlisted host.
func TestAllows(t *testing.T) {
	a := NewAllowlist(GitHub()...)
	cases := []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"GitHub.com", true},
		{"github.com:443", true},
		{"api.github.com:443", true},
		{"objects.githubusercontent.com", true},
		{"example.com", false},
		{"example.com:443", false},
		{"github.com.example.com", false},
		{"notgithub.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := a.Allows(c.host); got != c.want {
			t.Errorf("Allows(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// TestAllowsPortLiteral is the localhost model endpoint: the port is part of the
// permission, so allowing the name must not allow the rest of the box.
func TestAllowsPortLiteral(t *testing.T) {
	one := NewAllowlist("127.0.0.1:11434")
	for _, c := range []struct {
		host string
		want bool
	}{
		{"127.0.0.1:11434", true},
		{"127.0.0.1:8080", false},
		{"127.0.0.1", false},
	} {
		if got := one.Allows(c.host); got != c.want {
			t.Errorf("port literal Allows(%q) = %v, want %v", c.host, got, c.want)
		}
	}

	any := NewAllowlist("127.0.0.1")
	if !any.Allows("127.0.0.1:8080") {
		t.Error("bare host should allow any port on it")
	}
}

// TestDisallowedHostIsRefused is the ticket's done-when: a request to
// example.com is refused, the refusal names the host, and the pipeline reads it
// as exit 3.
func TestDisallowedHostIsRefused(t *testing.T) {
	c := Client(NewAllowlist(GitHub()...))
	_, err := c.Get("http://example.com/")
	if err == nil {
		t.Fatal("request to example.com was not refused")
	}
	var refused ErrHostNotAllowed
	if !errors.As(err, &refused) {
		t.Fatalf("error %v is not an ErrHostNotAllowed", err)
	}
	if refused.Host != "example.com" {
		t.Errorf("refused host = %q, want example.com", refused.Host)
	}
	if !strings.Contains(err.Error(), "example.com") {
		t.Errorf("error %q does not name the host", err)
	}
	if code := report.ExitCode(err); code != 3 {
		t.Errorf("ExitCode = %d, want 3", code)
	}
}

// TestAllowedHostPasses drives a real listener on an allowlisted literal. It is
// the offline form of "github.com passes": a test that reached the real service
// is exactly what PRD §6.2 says must not run unattended.
func TestAllowedHostPasses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	resp, err := Client(NewAllowlist(addr)).Get(srv.URL)
	if err != nil {
		t.Fatalf("allowlisted request refused: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestRedirectToDisallowedHostIsRefused pins the property the ticket asks for:
// a redirect is not a way out. The first host is allowed and the redirect is
// not; the hop is refused because its connection is a new dial, which is why no
// second check in CheckRedirect is needed.
func TestRedirectToDisallowedHostIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.RedirectHandler("http://example.com/", http.StatusFound))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	_, err := Client(NewAllowlist(addr)).Get(srv.URL)
	var refused ErrHostNotAllowed
	if !errors.As(err, &refused) {
		t.Fatalf("error %v is not the redirect refusal", err)
	}
	if refused.Host != "example.com" {
		t.Errorf("refused host = %q, want example.com", refused.Host)
	}
}

// TestProxyIsIgnored pins the decision that an environment proxy cannot move the
// connection off the allowlist. Asserted structurally because the proxy library
// bypasses proxies for loopback, so a request test would pass either way.
func TestProxyIsIgnored(t *testing.T) {
	tr := Client(NewAllowlist(GitHub()...)).Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("transport honours a proxy, which would check the proxy's host instead of the target's")
	}
}

// TestTransportKeepsDefaults pins what a transport literal would drop: without
// the default timeouts a stalled handshake or a body that never arrives pins the
// run, which is the failure §6.2 exists to prevent.
func TestTransportKeepsDefaults(t *testing.T) {
	tr := Client(NewAllowlist()).Transport.(*http.Transport)
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("TLS handshake timeout dropped")
	}
	if tr.IdleConnTimeout == 0 {
		t.Error("idle connection timeout dropped")
	}
	if tr.ExpectContinueTimeout == 0 {
		t.Error("expect-continue timeout dropped")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("HTTP/2 support dropped")
	}
}

// TestEndpointURLIsAllowed is the configuration contract: an endpoint is
// naturally written as a URL while the allowlist compares host:port, so a
// constructor that stored the URL verbatim would refuse every model call and
// still print a list that looked correct.
func TestEndpointURLIsAllowed(t *testing.T) {
	a := Default("https://api.example.com/v1")
	if !a.Allows("api.example.com:443") {
		t.Error("URL endpoint did not allow its own host")
	}
	if a.Allows("example.com:443") {
		t.Error("URL endpoint allowed a different host")
	}

	b := Default("http://127.0.0.1:11434/v1")
	if !b.Allows("127.0.0.1:11434") {
		t.Error("URL endpoint with a port did not allow that literal")
	}
	if b.Allows("127.0.0.1:8080") {
		t.Error("URL endpoint with a port allowed another port on the host")
	}
}

// TestDefaultModelEndpoint checks the second half of Default: GitHub is always
// allowed, and a blank setting allows no endpoint at all.
func TestDefaultModelEndpoint(t *testing.T) {
	a := Default("")
	if !a.Allows("github.com:443") {
		t.Error("Default with no endpoint should allow github.com")
	}
	if a.Allows("127.0.0.1:11434") {
		t.Error("Default with no endpoint allowed one that was never configured")
	}

	b := Default("127.0.0.1:11434")
	if !b.Allows("127.0.0.1:11434") {
		t.Error("Default(endpoint) does not allow its own endpoint")
	}
	if b.Allows("127.0.0.1:8080") {
		t.Error("Default(endpoint) allowed another port on the same host")
	}
}
