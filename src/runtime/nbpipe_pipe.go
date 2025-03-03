// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || darwin

package runtime

func nonblockingPipe() (r, w int32, errno int32) {
	r, w, errno = pipe() //系统调用，创建一个管道，返回两个fd，一个是用于读的fd，一个是用于写的fd，从w写入，只能从r读出
	if errno != 0 {
		return -1, -1, errno
	}
	//设置管道的读写为非阻塞
	closeonexec(r)
	setNonblock(r)
	closeonexec(w)
	setNonblock(w)
	return r, w, errno
}
