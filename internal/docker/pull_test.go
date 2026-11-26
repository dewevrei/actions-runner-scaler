package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

// pullStream writes the progress lines of a successful pull.
func pullStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintln(w, `{"status":"Pulling from library/busybox","id":"latest"}`)
	_, _ = fmt.Fprintln(w, `{"status":"Status: Downloaded newer image for busybox:latest"}`)
}

// TestImagePullRequest proves 0003 SPEC R320: the request is one
// POST /images/create with the name and the tag or digest that the Engine
// client derives from the reference, and with no registry credential and no
// platform.
func TestImagePullRequest(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	tests := []struct {
		ref, fromImage, tag string
	}{
		{"busybox", "docker.io/library/busybox", "latest"},
		{"a/b:1", "docker.io/a/b", "1"},
		{"a/b@sha256:" + hex64, "docker.io/a/b", "sha256:" + hex64},
		{"a/b:1@sha256:" + hex64, "docker.io/a/b", "sha256:" + hex64},
		{"localhost:5000/a/b:1", "localhost:5000/a/b", "1"},
		{"moby/buildkit:v0.35.1-rootless@sha256:" + hex64, "docker.io/moby/buildkit", "sha256:" + hex64},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			d := newFakeDaemon(t)
			var header http.Header
			d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
				if r.Method != http.MethodPost || path != "/images/create" {
					return false
				}
				header = r.Header.Clone()
				pullStream(w)
				return true
			}
			if err := d.client().ImagePull(context.Background(), tt.ref); err != nil {
				t.Fatalf("ImagePull: %v", err)
			}
			if got := d.callNames(); !reflect.DeepEqual(got, []string{"POST /images/create"}) {
				t.Fatalf("calls = %q, want one POST /images/create", got)
			}
			c, _ := d.find(http.MethodPost, "/images/create")
			if got := c.query.Get("fromImage"); got != tt.fromImage {
				t.Errorf("fromImage = %q, want %q", got, tt.fromImage)
			}
			if got := c.query.Get("tag"); got != tt.tag {
				t.Errorf("tag = %q, want %q", got, tt.tag)
			}
			if c.query.Has("platform") {
				t.Errorf("platform sent: %v", c.query)
			}
			if len(c.query) != 2 {
				t.Errorf("query = %v, want only fromImage and tag", c.query)
			}
			for _, name := range []string{"X-Registry-Auth", "X-Registry-Config"} {
				if _, ok := header[name]; ok {
					t.Errorf("request has a %s header", name)
				}
			}
		})
	}
}

// The stream is read to its end: a body larger than the socket buffers is
// written in full, with no write error, before ImagePull returns.
func TestImagePullReadsWholeStream(t *testing.T) {
	d := newFakeDaemon(t)
	var (
		mu       sync.Mutex
		writeErr error
		lines    int
	)
	d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
		if path != "/images/create" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		for i := range 20000 {
			_, err := fmt.Fprintf(w, `{"status":"Downloading","id":"layer-%d","progressDetail":{"current":%d,"total":100000}}`+"\n", i%50, i)
			mu.Lock()
			if err != nil && writeErr == nil {
				writeErr = err
			}
			lines++
			mu.Unlock()
		}
		return true
	}
	if err := d.client().ImagePull(context.Background(), "busybox"); err != nil {
		t.Fatalf("ImagePull: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if writeErr != nil || lines != 20000 {
		t.Errorf("server wrote %d lines, first error %v; want all 20000 with none", lines, writeErr)
	}
}

// pullOutcome checks a pull error: the op, whether it wraps ErrPullFailed,
// and the class that retry.Classify gives it.
func pullOutcome(t *testing.T, err error, failed bool, class retry.Class) {
	t.Helper()
	if err == nil {
		t.Fatal("ImagePull succeeded, want an error")
	}
	if opOf(err) != retry.OpImagePull {
		t.Errorf("op = %q, want %q (%v)", opOf(err), retry.OpImagePull, err)
	}
	if got := errors.Is(err, retry.ErrPullFailed); got != failed {
		t.Errorf("errors.Is(ErrPullFailed) = %v, want %v (%v)", got, failed, err)
	}
	if failed && errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a failed pull keeps context.DeadlineExceeded in its chain: %v", err)
	}
	if got := retry.Classify(context.Background(), retry.OpImagePull, err); got != class {
		t.Errorf("Classify = %v, want %v (%v)", got, class, err)
	}
}

// TestImagePull proves 0003 SPEC R320 with a fake Engine: the options are
// the zero value; the stream is waited for and the response closed; the
// failures that R320 sends to the error wait wrap ErrPullFailed and none of
// the others does.
func TestImagePull(t *testing.T) {
	const ref = "busybox:1"
	badJSON := json.Unmarshal([]byte("{x"), new(map[string]any))
	tests := []struct {
		name string
		// reqErr is what the request returns; waitErr what Wait returns.
		reqErr, waitErr error
		failed          bool
		class           retry.Class
		wantStatus      int
	}{
		{name: "success"},
		{name: "in-stream error", waitErr: errors.New("denied: requested access to the resource is denied"), failed: true, class: retry.Permanent},
		{name: "decode error", waitErr: badJSON, failed: true, class: retry.Permanent},
		{name: "stream read error", waitErr: fmt.Errorf("read: %w", net.ErrClosed), failed: true, class: retry.Permanent},
		{name: "deadline inside Wait", waitErr: fmt.Errorf("pull: %w", context.DeadlineExceeded), failed: true, class: retry.Permanent},
		{name: "deadline before the response", reqErr: fmt.Errorf("Post: %w", context.DeadlineExceeded), failed: true, class: retry.Permanent},
		{name: "transport error", reqErr: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, class: retry.Transient},
		{name: "502", reqErr: &StatusError{Status: 502, Err: errors.New("bad gateway")}, class: retry.Transient, wantStatus: 502},
		{name: "500", reqErr: &StatusError{Status: 500, Err: errors.New("boom")}, class: retry.Transient, wantStatus: 500},
		{name: "404", reqErr: &StatusError{Status: 404, Err: errors.New("manifest unknown")}, failed: true, class: retry.Permanent},
		{name: "404 by class", reqErr: cerrdefs.ErrNotFound.WithMessage("pull access denied"), failed: true, class: retry.Permanent},
		{name: "401", reqErr: &StatusError{Status: 401, Err: errors.New("unauthorized")}, failed: true, class: retry.Permanent},
		{name: "429", reqErr: &StatusError{Status: 429, Err: errors.New("toomanyrequests")}, failed: true, class: retry.Permanent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &fakePullResponse{waitErr: tt.waitErr}
			var (
				calls   int
				gotRef  string
				gotOpts client.ImagePullOptions
			)
			c := New(fakeEngine{pull: func(_ context.Context, r string, o client.ImagePullOptions) (client.ImagePullResponse, error) {
				calls++
				gotRef, gotOpts = r, o
				if tt.reqErr != nil {
					return nil, tt.reqErr
				}
				return resp, nil
			}})
			err := c.ImagePull(context.Background(), ref)

			if calls != 1 || gotRef != ref {
				t.Errorf("engine got %d calls for %q, want one for %q", calls, gotRef, ref)
			}
			if !reflect.DeepEqual(gotOpts, client.ImagePullOptions{}) {
				t.Errorf("options = %+v, want the zero value", gotOpts)
			}
			if tt.reqErr == nil && (resp.waits != 1 || resp.closes < 1) {
				t.Errorf("response waited %d times and closed %d times, want one wait and a close", resp.waits, resp.closes)
			}
			if tt.reqErr != nil && resp.waits != 0 {
				t.Errorf("Wait called after a failed request")
			}
			if tt.reqErr == nil && tt.waitErr == nil {
				if err != nil {
					t.Fatalf("ImagePull = %v, want nil", err)
				}
				return
			}
			pullOutcome(t, err, tt.failed, tt.class)
			if tt.wantStatus != 0 {
				var se *StatusError
				if !errors.As(err, &se) || se.Status != tt.wantStatus {
					t.Errorf("error has no StatusError with status %d: %v", tt.wantStatus, err)
				}
			}
			// Docker's text stays in the error for the error wait.
			cause := tt.reqErr
			if cause == nil {
				cause = tt.waitErr
			}
			if tt.failed && !strings.Contains(err.Error(), strings.TrimPrefix(cause.Error(), "status ")) {
				t.Errorf("error %q lacks Docker's text %q", err, cause)
			}
		})
	}
}

// A canceled ctx is Canceled for retry.Classify, never ErrPullFailed.
func TestImagePullCanceled(t *testing.T) {
	for name, c := range map[string]struct{ reqErr, waitErr error }{
		"request": {reqErr: fmt.Errorf("Post: %w", context.Canceled)},
		"Wait":    {waitErr: context.Canceled},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			resp := &fakePullResponse{waitErr: c.waitErr}
			cl := New(fakeEngine{pull: func(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error) {
				if c.reqErr != nil {
					return nil, c.reqErr
				}
				return resp, nil
			}})
			err := cl.ImagePull(ctx, "busybox")
			if err == nil || errors.Is(err, retry.ErrPullFailed) || !errors.Is(err, context.Canceled) {
				t.Fatalf("ImagePull = %v, want an error holding context.Canceled and not ErrPullFailed", err)
			}
			if got := retry.Classify(ctx, retry.OpImagePull, err); got != retry.Canceled {
				t.Errorf("Classify = %v, want Canceled", got)
			}
			if c.reqErr == nil && resp.closes < 1 {
				t.Error("response not closed")
			}
		})
	}
}

// A value that is not an image reference never reaches the Engine, and its
// failure does not read as a transport error. (A "sha256:<64 hex>" image ID
// parses as a name with a tag; telling IDs from references is config's
// ImageIsID, which the caller applies before it pulls.)
func TestImagePullBadReference(t *testing.T) {
	c := New(fakeEngine{pull: func(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error) {
		t.Error("the Engine was called")
		return nil, errors.New("unreachable")
	}})
	for _, ref := range []string{"", "Runner:latest", "a b", ":tag", strings.Repeat("a", 64)} {
		pullOutcome(t, c.ImagePull(context.Background(), ref), true, retry.Permanent)
	}
}

// TestImagePullDaemon proves the same sorting against a daemon that answers
// over HTTP: the status comes from the response, and the failures inside a
// 200 answer and the stream's end are told apart.
func TestImagePullDaemon(t *testing.T) {
	status := func(code int) func(w http.ResponseWriter, r *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) { writeError(w, code, "daemon says no") }
	}
	tests := []struct {
		name       string
		serve      func(w http.ResponseWriter, r *http.Request)
		drop       bool
		failed     bool
		class      retry.Class
		wantStatus int
		wantText   string
	}{
		{name: "404", serve: status(404), failed: true, class: retry.Permanent, wantText: "status 404: Error response from daemon: daemon says no"},
		{name: "401", serve: status(401), failed: true, class: retry.Permanent, wantText: "status 401"},
		{name: "429", serve: status(429), failed: true, class: retry.Permanent, wantText: "status 429"},
		{name: "500", serve: status(500), class: retry.Transient, wantStatus: 500},
		{name: "502", serve: status(502), class: retry.Transient, wantStatus: 502},
		{name: "transport error", drop: true, class: retry.Transient},
		{
			name: "in-stream error",
			serve: func(w http.ResponseWriter, r *http.Request) {
				pullStream(w)
				_, _ = fmt.Fprintln(w, `{"errorDetail":{"message":"denied: requested access to the resource is denied"},"error":"denied: requested access to the resource is denied"}`)
			},
			failed: true, class: retry.Permanent, wantText: "denied: requested access to the resource is denied",
		},
		{
			name: "decode error",
			serve: func(w http.ResponseWriter, r *http.Request) {
				pullStream(w)
				_, _ = fmt.Fprintln(w, `{"status": nope`)
			},
			failed: true, class: retry.Permanent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDaemon(t)
			if tt.drop {
				d.drop = func(method, path string) bool { return path == "/images/create" }
			}
			d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
				if path != "/images/create" || tt.serve == nil {
					return false
				}
				tt.serve(w, r)
				return true
			}
			err := d.client().ImagePull(context.Background(), "busybox")
			pullOutcome(t, err, tt.failed, tt.class)
			if tt.wantStatus != 0 {
				var se *StatusError
				if !errors.As(err, &se) || se.Status != tt.wantStatus {
					t.Errorf("error has no StatusError with status %d: %v", tt.wantStatus, err)
				}
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q lacks %q", err, tt.wantText)
			}
		})
	}
}

// A stream that stalls until the pull's deadline fails with ErrPullFailed,
// not with a deadline that Classify would call transient.
func TestImagePullDeadlineInStream(t *testing.T) {
	d := newFakeDaemon(t)
	d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
		if path != "/images/create" {
			return false
		}
		pullStream(w)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := d.client().ImagePull(ctx, "busybox")
	// pullOutcome classifies with a live context, as a caller does with the
	// parent of the pull's own deadline.
	pullOutcome(t, err, true, retry.Permanent)
}

// TestDistributionInspect proves 0003 SPEC R323 and R299: one
// GET /distribution/{name}/json with no registry credential; the platforms
// returned; a 401 and a 500 keep their status in an error for
// OpDistributionInspect.
func TestDistributionInspect(t *testing.T) {
	const ref = "moby/buildkit:v0.35.1-rootless"
	t.Run("platforms", func(t *testing.T) {
		d := newFakeDaemon(t)
		var header http.Header
		d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
			if r.Method != http.MethodGet || path != "/distribution/"+ref+"/json" {
				return false
			}
			header = r.Header.Clone()
			writeJSON(w, http.StatusOK, map[string]any{
				"Descriptor": map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json", "digest": "sha256:" + strings.Repeat("a", 64), "size": 100},
				"Platforms": []map[string]string{
					{"architecture": "amd64", "os": "linux"},
					{"architecture": "arm64", "os": "linux"},
				},
			})
			return true
		}
		got, err := d.client().DistributionInspect(context.Background(), ref)
		if err != nil {
			t.Fatalf("DistributionInspect: %v", err)
		}
		want := Distribution{Platforms: []ocispec.Platform{{Architecture: "amd64", OS: "linux"}, {Architecture: "arm64", OS: "linux"}}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("DistributionInspect = %+v, want %+v", got, want)
		}
		if names := d.callNames(); !reflect.DeepEqual(names, []string{"GET /distribution/" + ref + "/json"}) {
			t.Errorf("calls = %q, want one GET /distribution/%s/json", names, ref)
		}
		if _, ok := header["X-Registry-Auth"]; ok {
			t.Error("request has an X-Registry-Auth header")
		}
	})

	t.Run("no platforms", func(t *testing.T) {
		d := newFakeDaemon(t)
		d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
			writeJSON(w, http.StatusOK, map[string]any{"Descriptor": map[string]any{}})
			return true
		}
		got, err := d.client().DistributionInspect(context.Background(), ref)
		if err != nil || len(got.Platforms) != 0 {
			t.Errorf("DistributionInspect = %+v, %v; want no platforms and no error", got, err)
		}
	})

	for _, code := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("status %d", code), func(t *testing.T) {
			d := newFakeDaemon(t)
			d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
				writeError(w, code, "Failed authentication or no image found")
				return true
			}
			_, err := d.client().DistributionInspect(context.Background(), ref)
			if err == nil || opOf(err) != retry.OpDistributionInspect {
				t.Fatalf("DistributionInspect error = %v, op %q, want op %q", err, opOf(err), retry.OpDistributionInspect)
			}
			var se *StatusError
			if !errors.As(err, &se) || se.Status != code {
				t.Errorf("error has no StatusError with status %d: %v", code, err)
			}
			want := retry.Permanent
			if code >= 500 {
				want = retry.Transient
			}
			if got := retry.Classify(context.Background(), retry.OpDistributionInspect, err); got != want {
				t.Errorf("Classify = %v, want %v", got, want)
			}
		})
	}

	t.Run("zero options", func(t *testing.T) {
		var got client.DistributionInspectOptions
		c := New(fakeEngine{distribution: func(_ context.Context, _ string, o client.DistributionInspectOptions) (client.DistributionInspectResult, error) {
			got = o
			return client.DistributionInspectResult{}, nil
		}})
		if _, err := c.DistributionInspect(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		if got != (client.DistributionInspectOptions{}) {
			t.Errorf("options = %+v, want the zero value", got)
		}
	})
}
