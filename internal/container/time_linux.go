//go:build linux

package container

import (
	"time"

	"golang.org/x/sys/unix"
)

func timeOf(ts unix.Timespec) time.Time { return time.Unix(ts.Sec, ts.Nsec) }
