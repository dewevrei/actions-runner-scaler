package config

import (
	"fmt"
	"strings"
	"testing"
)

// imageCase is a value of runner.image or buildkit.image that 0003 SPEC
// R318 accepts; isID is whether it is a local image ID and not an image
// reference.
type imageCase struct {
	value string
	isID  bool
}

// imageCases returns the values R318 accepts and the ones it refuses.
func imageCases() (accepted []imageCase, refused []string) {
	hex64 := strings.Repeat("c", 64)
	accepted = []imageCase{
		{"sha256:" + hex64, true},
		{hex64, true},
		{"actions-runner-scaler-runner:local", false},
		{"busybox", false},
		{"moby/buildkit:v0.35.1-rootless@sha256:" + hex64, false},
		{"moby/buildkit@sha256:" + hex64, false},
		{"actions-runner-scaler-runner:local@sha256:" + hex64, false},
		{"localhost:5000/a/b:1", false},
		// A reference to the repository "sha256" with the tag "abc", which
		// is what ParseAnyReference returns.
		{"sha256:abc", false},
	}
	refused = []string{"", "Runner:latest", "a b", ":tag", "a@b"}
	return accepted, refused
}

// TestImageValue proves 0003 SPEC R318 and 0001 R28: both keys take an
// image ID or an image reference, a refused value gets exactly the R318
// text naming its key, and every error of a file is reported at once.
func TestImageValue(t *testing.T) {
	accepted, refused := imageCases()
	keys := []struct {
		key string
		set func(*Config, string)
	}{
		{"runner.image", func(c *Config, v string) { c.Runner.Image = v }},
		{"buildkit.image", func(c *Config, v string) { c.BuildKit.Image = v }},
	}
	for _, k := range keys {
		for _, a := range accepted {
			t.Run(k.key+" accepts "+a.value, func(t *testing.T) {
				if err := CheckImage(a.value); err != nil {
					t.Fatalf("CheckImage = %v, want nil", err)
				}
				if got := ImageIsID(a.value); got != a.isID {
					t.Errorf("ImageIsID = %v, want %v", got, a.isID)
				}
				cfg := validConfig(t)
				k.set(&cfg, a.value)
				if err := cfg.Validate(); err != nil {
					t.Errorf("Validate = %v, want nil", err)
				}
			})
		}
		for _, v := range refused {
			t.Run(k.key+" refuses "+v, func(t *testing.T) {
				if err := CheckImage(v); err != ErrImageForm {
					t.Fatalf("CheckImage = %v, want ErrImageForm", err)
				}
				if ImageIsID(v) {
					t.Error("ImageIsID = true, want false")
				}
				cfg := validConfig(t)
				k.set(&cfg, v)
				want := fmt.Sprintf("%s: must be an image ID or an image reference <name>[:<tag>][@<digest>], got %q", k.key, v)
				if got := errorTexts(cfg.Validate()); len(got) != 1 || got[0] != want {
					t.Errorf("Validate = %q, want only %q", got, want)
				}
			})
		}
	}
}

// Both keys and a third key fail in one report, from a file.
func TestImageValueReportedAtOnce(t *testing.T) {
	key := writeKey(t, pkcs1PEM(t), 0o600)
	content := requiredTOML(key)
	content = strings.Replace(content, `image = "`+testRunnerImage+`"`, `image = "Runner:latest"`, 1)
	content = strings.Replace(content, `image = "`+testBuildKitImage+`"`, `image = ":tag"`, 1)
	content = withBuildKit(content, "cpus = 0\n")
	_, err := loadString(t, content)
	want := []string{
		`runner.image: must be an image ID or an image reference <name>[:<tag>][@<digest>], got "Runner:latest"`,
		`buildkit.image: must be an image ID or an image reference <name>[:<tag>][@<digest>], got ":tag"`,
		`buildkit.cpus: must be at least 0.01 and fit in int64 nanoCPUs, got 0`,
	}
	got := errorTexts(err)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Load errors:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// ImageIsID reads a value the way CheckImage does, so a value that
// CheckImage refuses is never an ID.
func TestImageIsIDRefusedValues(t *testing.T) {
	for _, v := range []string{"", "sha256:", "sha256:" + strings.Repeat("g", 64), strings.Repeat("c", 63), strings.Repeat("c", 65)} {
		if CheckImage(v) != nil && ImageIsID(v) {
			t.Errorf("ImageIsID(%q) = true for a refused value", v)
		}
	}
}
