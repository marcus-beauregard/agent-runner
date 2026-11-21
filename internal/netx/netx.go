// Package netx draws the network boundary PRD §6.2 asks for: a run may reach
// github.com and its model endpoint, and nothing else. The check lives in the
// client a run's in-process calls share, so a URL that came out of an issue body
// cannot become a request through that client.
//
// It checks the name a request asks for, not where that name resolves. DNS
// rebinding is therefore out of scope here and is answered by the network
// namespace in the container sandbox (§6.2). Proxy environment variables are
// ignored for the same reason: a proxy receives the connection in place of the
// host being fetched, so the host checked would stop being the host fetched.
package netx

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/marcus-beauregard/agent-runner/internal/report"
)

// maxRedirects bounds a redirect chain. The http.Client default is ten; naming
// it here keeps the number in one place instead of relying on a zero value.
const maxRedirects = 10

// GitHub is the allowlist for in-process GitHub reads: the hosts the API and
// its downloads live on. It is not what confines gh or git — those run as child
// processes this client cannot see, and their egress is the container's job
// (§6.2). It returns a fresh slice so a caller can append the model endpoint
// without mutating another caller's list.
func GitHub() []string {
	return []string{
		"github.com",
		"api.github.com",
		"codeload.github.com",
		"objects.githubusercontent.com",
	}
}

// Default is the allowlist a run starts from: GitHub, plus the model endpoint it
// was configured with. The endpoint may be written as a URL or as a host, since
// config holds one and the allowlist compares the other; an empty endpoint adds
// nothing, because a blank setting must not open the net.
func Default(modelEndpoint string) Allowlist {
	entries := GitHub()
	if modelEndpoint = strings.TrimSpace(modelEndpoint); modelEndpoint != "" {
		entries = append(entries, modelEndpoint)
	}
	return NewAllowlist(entries...)
}

// Allowlist is the set of destinations one run may reach. The zero value allows
// nothing, which is the direction a zero value should fail in.
type Allowlist struct {
	hosts map[string]bool
}

// NewAllowlist allows each entry. A bare host name ("github.com") allows that
// host on any port; a host:port entry ("127.0.0.1:11434") allows exactly that
// destination. The literal form exists for a model endpoint on localhost, where
// allowing the name would allow every service bound there.
//
// An entry written as a URL ("https://api.example.com/v1") is reduced to its
// host, because that is the form a configured endpoint naturally takes and the
// form a request names. Empty entries are skipped, so a missing setting cannot
// quietly become an allow-all.
func NewAllowlist(entries ...string) Allowlist {
	a := Allowlist{hosts: make(map[string]bool, len(entries))}
	for _, e := range entries {
		e = entryHost(e)
		if e == "" {
			continue
		}
		a.hosts[e] = true
	}
	return a
}

// entryHost reduces one configured entry to the host, or host:port, a request
// will name. Anything else is returned as written: an entry that matches nothing
// refuses rather than allows, and a typo that opens the net is the only failure
// this must not have.
func entryHost(entry string) string {
	entry = strings.ToLower(strings.TrimSpace(entry))
	if strings.Contains(entry, "://") {
		if u, err := url.Parse(entry); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return entry
}

// Allows reports whether host — a name, or a literal host:port — is on the list.
// The comparison is an exact lookup, never a suffix match: allowing github.com
// must not allow github.com.example.com, which is the shape of a name an
// attacker registers precisely to be read as an allowlisted host.
func (a Allowlist) Allows(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	if a.hosts[host] {
		return true
	}
	name, _, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	return a.hosts[name]
}

// ErrHostNotAllowed is the refusal: the request named a destination outside the
// allowlist. Host and Port are what was asked for, because "refused" without the
// name leaves the operator guessing what the run was trying to do.
//
// It unwraps to report.ErrRefused, so however deep the error travelled the
// pipeline exits 3 without every caller having to remember the mapping.
type ErrHostNotAllowed struct {
	Host string
	Port string
}

// Error names the destination that was refused.
func (e ErrHostNotAllowed) Error() string {
	dest := e.Host
	if e.Port != "" {
		dest = net.JoinHostPort(e.Host, e.Port)
	}
	return fmt.Sprintf("%v: host not allowed: %s", report.ErrRefused, dest)
}

// Unwrap reports the sentinel the pipeline maps to exit 3.
func (e ErrHostNotAllowed) Unwrap() error { return report.ErrRefused }

// Client returns an http.Client that refuses every destination outside a.
//
// The guard is DialContext, and it needs no second copy on redirect: the
// transport dials the host of every new connection, redirect hops included, and
// a pooled connection can only belong to a host that was allowed when it was
// dialled. CheckRedirect is left to bound the chain.
//
// The transport is http.DefaultTransport with two fields changed, not a bare
// literal: the default carries the handshake, header and idle timeouts that keep
// a stalled server from pinning a run that is supposed to never hang unattended.
func Client(a Allowlist) *http.Client {
	dialer := &net.Dialer{}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// Nil, deliberately: a proxy from the environment takes the connection in
	// place of the target, so DialContext would check the proxy's address and
	// the allowlist would stop meaning anything.
	tr.Proxy = nil
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !a.Allows(addr) {
			return nil, refusal(addr)
		}
		return dialer.DialContext(ctx, network, addr)
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
}

// refusal turns a dial address into the named refusal.
func refusal(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ErrHostNotAllowed{Host: addr}
	}
	return ErrHostNotAllowed{Host: host, Port: port}
}
