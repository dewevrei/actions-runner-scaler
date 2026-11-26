package docker

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/dewevrei/actions-runner-scaler/internal/retry"
)

func TestImageInspect(t *testing.T) {
	const ref = RunnerImageTag
	t.Run("found", func(t *testing.T) {
		d := newFakeDaemon(t)
		d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
			if path != "/images/"+ref+"/json" {
				return false
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"Id":           "sha256:abc",
				"Architecture": "arm64",
				"Config":       map[string]any{"Labels": map[string]string{"a": "b"}},
			})
			return true
		}
		img, ok, err := d.client().ImageInspect(context.Background(), ref)
		if err != nil || !ok {
			t.Fatalf("ImageInspect = %v, %v; want found", ok, err)
		}
		want := Image{ID: "sha256:abc", Architecture: "arm64", Labels: map[string]string{"a": "b"}}
		if !reflect.DeepEqual(img, want) {
			t.Errorf("image = %+v, want %+v", img, want)
		}
		var n int
		for _, name := range d.callNames() {
			if strings.Contains(name, "/images/") {
				n++
			}
			if strings.Contains(name, "/images/create") {
				t.Errorf("pulled: %s", name)
			}
		}
		if n != 1 {
			t.Errorf("%d image calls, want 1: %v", n, d.callNames())
		}
	})

	t.Run("no config", func(t *testing.T) {
		d := newFakeDaemon(t)
		d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
			writeJSON(w, http.StatusOK, map[string]string{"Id": "sha256:abc", "Architecture": "amd64"})
			return true
		}
		img, ok, err := d.client().ImageInspect(context.Background(), ref)
		if err != nil || !ok || img.Labels != nil || img.Architecture != "amd64" {
			t.Errorf("ImageInspect = %+v, %v, %v; want found, amd64, no labels", img, ok, err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		d := newFakeDaemon(t)
		d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
			writeError(w, http.StatusNotFound, "No such image")
			return true
		}
		img, ok, err := d.client().ImageInspect(context.Background(), ref)
		if err != nil || ok || !reflect.DeepEqual(img, Image{}) {
			t.Errorf("ImageInspect = %+v, %v, %v; want zero image, false, nil", img, ok, err)
		}
	})

	t.Run("other error", func(t *testing.T) {
		d := newFakeDaemon(t)
		d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
			writeError(w, http.StatusInternalServerError, "boom")
			return true
		}
		_, ok, err := d.client().ImageInspect(context.Background(), ref)
		if err == nil || ok {
			t.Fatalf("ImageInspect = %v, %v; want an error", ok, err)
		}
		if opOf(err) != retry.OpImageInspect {
			t.Errorf("op = %q, want %q", opOf(err), retry.OpImageInspect)
		}
	})
}

func TestHasRunnerImageLabels(t *testing.T) {
	const (
		source = "org.opencontainers.image.source"
		title  = "org.opencontainers.image.title"
		src    = "https://github.com/dewevrei/actions-runner-scaler"
		ttl    = "actions-runner-scaler-runner"
	)
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"both exact", map[string]string{source: src, title: ttl}, true},
		{"both exact with others", map[string]string{source: src, title: ttl, "x": "y"}, true},
		{"source only", map[string]string{source: src}, false},
		{"title only", map[string]string{title: ttl}, false},
		{"base image source", map[string]string{source: "https://github.com/actions/runner", title: ttl}, false},
		{"extra whitespace", map[string]string{source: src + " ", title: ttl}, false},
		{"leading whitespace in title", map[string]string{source: src, title: " " + ttl}, false},
		{"nil", nil, false},
		{"empty", map[string]string{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasRunnerImageLabels(tt.labels); got != tt.want {
				t.Errorf("HasRunnerImageLabels(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}
