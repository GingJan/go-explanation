// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || darwin || dragonfly || freebsd || (js && wasm) || linux || netbsd || openbsd || solaris

package poll

import "syscall"

// CloseFunc is used to hook the close call.
// CloseFunc 用于和系统调用close挂钩
var CloseFunc func(int) error = syscall.Close

// AcceptFunc is used to hook the accept call.
// CloseFunc 用于和系统调用accept挂钩
var AcceptFunc func(int) (int, syscall.Sockaddr, error) = syscall.Accept
