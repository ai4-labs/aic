//go:build !unix

package main

import "github.com/ai4-labs/aic/internal/proto"

func watchResize(int) (<-chan proto.Size, func()) { return nil, func() {} }
