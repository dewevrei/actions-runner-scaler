package config

import (
	// go-digest accepts a digest algorithm only when its hash is linked
	// into the binary. Without sha256, ParseAnyReference reads
	// sha256:<64 hex digits> as a name with a tag and fails on
	// <name>@sha256:<64 hex digits> (0003 PLAN M42a).
	_ "crypto/sha256"
	"errors"

	"github.com/distribution/reference"
)

// ErrImageForm is the error CheckImage returns. Its text does not quote the
// value, so a caller that may not echo the value (init's answers) can print
// it as is.
var ErrImageForm = errors.New("must be an image ID or an image reference <name>[:<tag>][@<digest>]")

// CheckImage checks a runner.image or buildkit.image value (0003 SPEC R318):
// a local image ID (sha256:<64 hex digits> or the 64 hex digits alone) or an
// image reference <name>[:<tag>][@<digest>], read by reference.ParseAnyReference.
func CheckImage(s string) error {
	if _, err := reference.ParseAnyReference(s); err != nil {
		return ErrImageForm
	}
	return nil
}

// ImageIsID reports whether the image value s is a local image ID, which no
// registry can supply, and not an image reference that may be pulled (0003
// SPEC R318, R319). The value must have passed CheckImage; any other value
// reports false.
func ImageIsID(s string) bool {
	ref, err := reference.ParseAnyReference(s)
	if err != nil {
		return false
	}
	_, named := ref.(reference.Named)
	return !named
}
