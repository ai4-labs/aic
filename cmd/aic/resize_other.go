//go:build !unix

package main

import "github.com/rakyll/ai-container/internal/proto"

func watchResize(int) (<-chan proto.Size, func()) { return nil, func() {} }
