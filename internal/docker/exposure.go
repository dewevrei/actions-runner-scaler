package docker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// ExposedAddr is where Docker Desktop's "Expose daemon on
// tcp://localhost:2375 without TLS" setting listens, as SPEC Design (Docker
// exposure check, R79) fixes it.
//
// Only the IPv4 loopback is checked. Docker's documentation names the
// address of the setting as "tcp://localhost:2375" and does not say whether
// it also binds [::1]:2375 (docs.docker.com/desktop/settings-and-maintenance/
// settings/, "General"), so that is unconfirmed. Checking [::1] as well would
// change the R79 rule, which is a SPEC change.
const ExposedAddr = "127.0.0.1:2375"

// DialFunc opens a connection, like (*net.Dialer).DialContext.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// pingBodyLimit bounds how much of a reply is read; the daemon answers "OK".
const pingBodyLimit = 64

// Exposure is the result of CheckExposure.
type Exposure int

const (
	// NotExposed: the connection was refused, or something answered that is
	// not an unauthenticated daemon.
	NotExposed Exposure = iota
	// Exposed: GET /_ping answered 200 with the body "OK".
	Exposed
	// ExposureUnknown: ctx ended, or the connection timed out, before an
	// answer. R79 treats it like NotExposed; callers can log it.
	ExposureUnknown
)

// String returns "not exposed", "exposed" or "unknown".
func (e Exposure) String() string {
	switch e {
	case NotExposed:
		return "not exposed"
	case Exposed:
		return "exposed"
	case ExposureUnknown:
		return "unknown"
	}
	return "Exposure(" + strconv.Itoa(int(e)) + ")"
}

// CheckExposure reports whether a Docker daemon answers without
// authentication on ExposedAddr (R79): GET /_ping returns 200 with the body
// "OK" (surrounding white space ignored). Only Exposed should stop the
// scaler. A refused connection, any other reply or any error is
// NotExposed, except that ctx ending or a timeout before the answer is
// ExposureUnknown, so the caller can tell the check did not finish.
//
// dial opens the TCP connection to ExposedAddr; nil means a plain
// net.Dialer. CheckExposure never uses a proxy and does not time out by
// itself; bound it with ctx.
func CheckExposure(ctx context.Context, dial DialFunc) Exposure {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	hc := &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dial(ctx, "tcp", ExposedAddr)
			},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer hc.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ExposedAddr+"/_ping", nil)
	if err != nil {
		return NotExposed
	}
	resp, err := hc.Do(req)
	if err != nil {
		return failed(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return NotExposed
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, pingBodyLimit))
	if err != nil {
		return failed(ctx, err)
	}
	if strings.TrimSpace(string(body)) == "OK" {
		return Exposed
	}
	return NotExposed
}

// failed returns ExposureUnknown if err came from ctx ending or a timeout,
// and NotExposed otherwise.
func failed(ctx context.Context, err error) Exposure {
	if ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded) {
		return ExposureUnknown
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ExposureUnknown
	}
	return NotExposed
}
