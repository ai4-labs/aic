//go:build !linux

package container

import (
	"context"
	"errors"
	"io"

	"github.com/rakyll/ai-container/internal/proto"
)

// Init is a no-op outside of Linux.
func Init() {}

// Run is only available on Linux.
func Run(ctx context.Context, spec proto.Spec, image io.Reader, rio proto.IO) (proto.Result, error) {
	return proto.Result{}, errors.New("containers require Linux")
}
