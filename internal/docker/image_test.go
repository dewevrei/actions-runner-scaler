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
			})
			return true
		}
		img, ok, err := d.client().ImageInspect(context.Background(), ref)
		if err != nil || !ok {
			t.Fatalf("ImageInspect = %v, %v; want found", ok, err)
		}
		want := Image{ID: "sha256:abc", Architecture: "arm64"}
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

// 0003 SPEC R319, R322: the lookup sends the configured value as the image
// name as it is, whether an ID, a tag or a reference with a digest, and
// never pulls.
func TestImageInspectReferenceForms(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	for _, ref := range []string{
		"sha256:" + hex64,
		hex64,
		RunnerImageTag,
		"busybox",
		"localhost:5000/a/b:1",
		"moby/buildkit:v0.35.1-rootless@sha256:" + hex64,
		RunnerImageTag + "@sha256:" + hex64,
	} {
		d := newFakeDaemon(t)
		d.handle = func(w http.ResponseWriter, r *http.Request, path string) bool {
			if r.Method != http.MethodGet || path != "/images/"+ref+"/json" {
				return false
			}
			writeJSON(w, http.StatusOK, map[string]string{"Id": "sha256:" + hex64, "Architecture": "arm64"})
			return true
		}
		img, ok, err := d.client().ImageInspect(context.Background(), ref)
		if err != nil || !ok || img.ID != "sha256:"+hex64 {
			t.Errorf("ImageInspect(%q) = %+v, %v, %v; want the image", ref, img, ok, err)
		}
		if got := d.callNames(); len(got) != 1 || got[0] != "GET /images/"+ref+"/json" {
			t.Errorf("ImageInspect(%q) made the calls %q, want one GET", ref, got)
		}
	}
}
