//go:build !windows

package app

import (
	"context"
	"errors"
)

func mountVHDX(context.Context, string, string, bool) (string, error) {
	return "", errors.New("VHDX mounting is supported only on Windows")
}

func preflightVHDX(context.Context, string) error { return nil }

func detachVHDX(context.Context, string) (bool, string, error) {
	return false, "", errors.New("VHDX detaching is supported only on Windows")
}
