// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || darwin || dragonfly || freebsd || (js && wasm) || linux || netbsd || openbsd || solaris

package poll

import "syscall"

// CloseFunc is used to hook the close call.
// CloseFunc 挂靠 close
var CloseFunc func(int) error = syscall.Close

// AcceptFunc is used to hook the accept call.
// AcceptFunc 挂靠 syscall.Accept
var AcceptFunc func(int) (int, syscall.Sockaddr, error) = syscall.Accept
