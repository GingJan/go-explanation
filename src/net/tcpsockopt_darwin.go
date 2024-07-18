// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import (
	"runtime"
	"syscall"
	"time"
)

// syscall.TCP_KEEPINTVL is missing on some darwin architectures.
const sysTCP_KEEPINTVL = 0x101

func setKeepAlivePeriod(fd *netFD, d time.Duration) error {
	// The kernel expects seconds so round to next highest second.
	secs := int(roundDurationUp(d, time.Second))
	if err := fd.pfd.SetsockoptInt(syscall.IPPROTO_TCP, sysTCP_KEEPINTVL, secs); err != nil {//// 设置 Keep-Alive 探测报文发送间隔（秒）
		return wrapSyscallError("setsockopt", err)
	}
	err := fd.pfd.SetsockoptInt(syscall.IPPROTO_TCP, syscall.TCP_KEEPALIVE, secs)//设置 Keep-Alive 探测间隔（秒）
	runtime.KeepAlive(fd)
	return wrapSyscallError("setsockopt", err)
}
