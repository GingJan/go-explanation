// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || 386

package runtime

import (
	"internal/goarch"
	"unsafe"
)

// adjust Gobuf as if it executed a call to fn with context ctxt
// and then stopped before the first instruction in fn.
func gostartcall(buf *gobuf, fn, ctxt unsafe.Pointer) {
	sp := buf.sp//该协程的栈顶地址
	sp -= goarch.PtrSize//
	*(*uintptr)(unsafe.Pointer(sp)) = buf.pc//pc区域和栈区域是相邻的吗？
	buf.sp = sp
	buf.pc = uintptr(fn)//该协程从fn地址开始执行
	buf.ctxt = ctxt
}
