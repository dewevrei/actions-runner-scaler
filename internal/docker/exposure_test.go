package docker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

// redirect returns a DialFunc that connects to addr instead and records
// what CheckExposure asked for.
func redirect(addr string, asked *[]string, mu *sync.Mutex) DialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		*asked = append(*asked, network+" "+address)
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
}

func TestCheckExposure(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    Exposure
	}{
		{"daemon answers OK", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/_ping" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Api-Version", "1.56")
			_, _ = io.WriteString(w, "OK")
		}, Exposed},
		{"OK with a newline", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "OK\n")
		}, Exposed},
		{"other body", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "hello")
		}, NotExposed},
		{"empty body", func(w http.ResponseWriter, r *http.Request) {}, NotExposed},
		{"long body starting with OK", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "OK but not a daemon")
		}, NotExposed},
		{"404", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}, NotExposed},
		{"401 with OK", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "OK")
		}, NotExposed},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}, NotExposed},
		{"connection dropped before the answer", func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		}, NotExposed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			var (
				mu    sync.Mutex
				asked []string
			)
			got := CheckExposure(context.Background(), redirect(srv.Listener.Addr().String(), &asked, &mu))
			if got != tt.want {
				t.Errorf("CheckExposure = %v, want %v", got, tt.want)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(asked) != 1 || asked[0] != "tcp 127.0.0.1:2375" {
				t.Errorf("dialed %q, want one dial to tcp 127.0.0.1:2375", asked)
			}
		})
	}
}

func TestCheckExposureNoListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // connection refused from now on

	var (
		mu    sync.Mutex
		asked []string
	)
	if got := CheckExposure(context.Background(), redirect(addr, &asked, &mu)); got != NotExposed {
		t.Errorf("CheckExposure = %v with no listener, want %v", got, NotExposed)
	}
}

func TestCheckExposureDialError(t *testing.T) {
	dial := func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dial failed")
	}
	if got := CheckExposure(context.Background(), dial); got != NotExposed {
		t.Errorf("CheckExposure = %v on a dial error, want %v", got, NotExposed)
	}
}

// A daemon too slow to answer before ctx ends is unknown, not exposed, and
// CheckExposure returns soon after ctx ends.
func TestCheckExposureHonorsContext(t *testing.T) {
	// A listener that accepts and never answers.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	defer func() {
		select {
		case c := <-accepted:
			c.Close()
		default:
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var (
		mu    sync.Mutex
		asked []string
	)
	start := time.Now()
	if got := CheckExposure(ctx, redirect(ln.Addr().String(), &asked, &mu)); got != ExposureUnknown {
		t.Errorf("CheckExposure = %v for a silent listener, want %v", got, ExposureUnknown)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("CheckExposure took %v after the context ended", d)
	}
}

func TestCheckExposureIgnoresProxy(t *testing.T) {
	// A proxy in the environment must not see the request.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("proxy got %s %s", r.Method, r.URL)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "OK")
	}))
	defer srv.Close()
	var (
		mu    sync.Mutex
		asked []string
	)
	if got := CheckExposure(context.Background(), redirect(srv.Listener.Addr().String(), &asked, &mu)); got != Exposed {
		t.Errorf("CheckExposure = %v, want %v", got, Exposed)
	}
}

// A dial that times out on its own (not through ctx) is unknown.
func TestCheckExposureDialTimeout(t *testing.T) {
	dial := func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	}
	if got := CheckExposure(context.Background(), dial); got != ExposureUnknown {
		t.Errorf("CheckExposure = %v on a dial timeout, want %v", got, ExposureUnknown)
	}
}

// The body arrives after ctx ends: the status said 200, but without the
// body there is no verdict.
func TestCheckExposureBodyTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "OK")
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var (
		mu    sync.Mutex
		asked []string
	)
	if got := CheckExposure(ctx, redirect(srv.Listener.Addr().String(), &asked, &mu)); got != ExposureUnknown {
		t.Errorf("CheckExposure = %v, want %v", got, ExposureUnknown)
	}
}

func TestExposureString(t *testing.T) {
	for e, want := range map[Exposure]string{
		NotExposed:      "not exposed",
		Exposed:         "exposed",
		ExposureUnknown: "unknown",
		Exposure(7):     "Exposure(7)",
	} {
		if got := e.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(e), got, want)
		}
	}
}
