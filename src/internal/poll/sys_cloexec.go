// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements accept for platforms that do not provide a fast path for
// setting SetNonblock and CloseOnExec.

//go:build aix || darwin || (js && wasm) || (solaris && !illumos)

package poll

import (
	"syscall"
)

// Wrapper around the accept system call that marks the returned file
// descriptor as nonblocking and close-on-exec.
// 入参s是fd，调用系统accept，接收新连接请求，并把新连接client_fd设置为非阻塞 & close-on-exec
func accept(s int) (int, syscall.Sockaddr, string, error) {
	// See ../syscall/exec_unix.go for description of ForkLock.
	// It is probably okay to hold the lock across syscall.Accept
	// because we have put fd.sysfd into non-blocking mode.
	// However, a call to the File method will put it back into
	// blocking mode. We can't take that risk, so no use of ForkLock here.
	ns, sa, err := AcceptFunc(s) //系统调用 accept，因为s是非阻塞fd，所以当没有连接时会立即返回，此时err可能为 syscall.EAGAIN ；若有新连接建立，则ns=新的client_fd
	if err == nil {
		syscall.CloseOnExec(ns)
	}
	if err != nil {
		return -1, nil, "accept", err
	}

	if err = syscall.SetNonblock(ns, true); err != nil { //把ns（新连接fd）设置为非阻塞
		CloseFunc(ns) //系统调用 close 关闭连接/socket
		return -1, nil, "setnonblock", err
	}
	return ns, sa, "", nil
}
