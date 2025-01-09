// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows || solaris

package poll

import (
	"errors"
	"sync"
	"syscall"
	"time"
	_ "unsafe" // for go:linkname
)

// runtimeNano returns the current value of the runtime clock in nanoseconds.
//go:linkname runtimeNano runtime.nanotime
func runtimeNano() int64

func runtime_pollServerInit()                            //相当于epoll_create()，初始化/创建epoll实例
func runtime_pollOpen(fd uintptr) (uintptr, int)         //相当于epoll_add(ADD)
func runtime_pollClose(ctx uintptr)                      //相当于epoll_add(DEL)，把pd指向的底层fd从epoll的监听队列移除
func runtime_pollWait(ctx uintptr, mode int) int         //相当于epoll_wait，对应底层的 runtime.poll_runtime_pollWait 函数
func runtime_pollWaitCanceled(ctx uintptr, mode int) int //本函数只用于windows系统。指向 runtime.poll_runtime_pollWaitCanceled，调用本函数，返回bool或阻塞等待IO
func runtime_pollReset(ctx uintptr, mode int) int        //重置runtime.pollDesc，以便后续复用。指向 runtime.poll_runtime_pollReset
func runtime_pollSetDeadline(ctx uintptr, d int64, mode int)
func runtime_pollUnblock(ctx uintptr) //解除在该fd上等待事件的所有G的阻塞，指向 runtime.poll_runtime_pollUnblock
func runtime_isPollServerDescriptor(fd uintptr) bool

//是底层系统fd的封装，负责fd和epoll的交互
type pollDesc struct {
	runtimeCtx uintptr //指向runtime.pollDesc，而runtime.pollDesc则是指向底层系统fd的封装，当为0时说明该poll.pollDesc是不可pollable的
}

var serverInit sync.Once

//创建并初始化epoll实例，同时操作epoll添加对fd.Sysfd的监听
func (pd *pollDesc) init(fd *FD) error {
	serverInit.Do(runtime_pollServerInit)             //调用对应系统平台的epoll_create，创建并初始化epoll实例
	ctx, errno := runtime_pollOpen(uintptr(fd.Sysfd)) //把fd.Sysfd指向的系统fd添加到epoll的监听队列里，底层是把fd对应的系统fd添加到epoll的监听队列里，返回的ctx是代表fd的对象（该fd已被监听，使用了 runtime.pollDesc实例 来代表被监听的fd（封装））
	if errno != 0 {
		return errnoErr(syscall.Errno(errno))
	}
	pd.runtimeCtx = ctx
	return nil
}

//把pd.runtimeCtx底层的fd从epoll的监听队列移除，并归还底层runtime.pollDesc实例到复用池
func (pd *pollDesc) close() {
	if pd.runtimeCtx == 0 {
		return
	}
	runtime_pollClose(pd.runtimeCtx) //把pd.runtimeCtx底层的fd从监听队列移除，并归还底层runtime.pollDesc对象到复用池
	pd.runtimeCtx = 0
}

// Evict evicts fd from the pending list, unblocking any I/O running on fd.
// 解除等待在此fd上所有G的阻塞
func (pd *pollDesc) evict() {
	if pd.runtimeCtx == 0 {
		return
	}
	runtime_pollUnblock(pd.runtimeCtx)
}

//重置pd里面的runtimeCtx，以便后续使用
//一般调用本方法后，接着会调用waitRead或waitWrite方法
func (pd *pollDesc) prepare(mode int, isFile bool) error {
	if pd.runtimeCtx == 0 {
		return nil
	}
	res := runtime_pollReset(pd.runtimeCtx, mode) //重置runtime.pollDesc，以便后续复用
	return convertErr(res, isFile)
}

func (pd *pollDesc) prepareRead(isFile bool) error {
	return pd.prepare('r', isFile)
}

func (pd *pollDesc) prepareWrite(isFile bool) error {
	return pd.prepare('w', isFile)
}

func (pd *pollDesc) wait(mode int, isFile bool) error {
	if pd.runtimeCtx == 0 {
		return errors.New("waiting for unsupported file type")
	}
	res := runtime_pollWait(pd.runtimeCtx, mode) //阻塞在此，直到底层fd上的读写事件就绪
	return convertErr(res, isFile)               //判断是否有异常
}

//所有调用本方法的调用方都可能会被阻塞，直到底层fd的读写事件就绪
func (pd *pollDesc) waitRead(isFile bool) error {
	return pd.wait('r', isFile)
}

func (pd *pollDesc) waitWrite(isFile bool) error {
	return pd.wait('w', isFile)
}

//本方法只用于windows系统
func (pd *pollDesc) waitCanceled(mode int) {
	if pd.runtimeCtx == 0 {
		return
	}
	runtime_pollWaitCanceled(pd.runtimeCtx, mode)
}

// 判断当前pd是否pollable的
func (pd *pollDesc) pollable() bool {
	return pd.runtimeCtx != 0
}

// Error values returned by runtime_pollReset and runtime_pollWait.
// These must match the values in runtime/netpoll.go.
// 由runtime_pollReset 和 runtime_pollWait 返回的错误值，必须和runtime/netpoll.go的错误值一致
const (
	pollNoError        = 0
	pollErrClosing     = 1
	pollErrTimeout     = 2
	pollErrNotPollable = 3
)

func convertErr(res int, isFile bool) error {
	switch res {
	case pollNoError:
		return nil
	case pollErrClosing:
		return errClosing(isFile)
	case pollErrTimeout:
		return ErrDeadlineExceeded
	case pollErrNotPollable:
		return ErrNotPollable
	}
	println("unreachable: ", res)
	panic("unreachable")
}

// SetDeadline 设置该FD实例等待读和写的超时时间
func (fd *FD) SetDeadline(t time.Time) error {
	return setDeadlineImpl(fd, t, 'r'+'w')
}

// SetReadDeadline 设置该FD实例等待读的超时时间
func (fd *FD) SetReadDeadline(t time.Time) error {
	return setDeadlineImpl(fd, t, 'r')
}

// SetReadDeadline 设置该FD实例等待写的超时时间
func (fd *FD) SetWriteDeadline(t time.Time) error {
	return setDeadlineImpl(fd, t, 'w')
}

// setDeadlineImpl 设置底层fd等待读/写的超时时间
func setDeadlineImpl(fd *FD, t time.Time, mode int) error {
	var d int64
	if !t.IsZero() {
		d = int64(time.Until(t))
		if d == 0 {
			d = -1 // don't confuse deadline right now with no deadline
		}
	}
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	if fd.pd.runtimeCtx == 0 {
		return ErrNoDeadline
	}
	runtime_pollSetDeadline(fd.pd.runtimeCtx, d, mode)
	return nil
}

// IsPollDescriptor 判断底层fd是否被poller（epoll）监听中
// 本函数只用于测试阶段
func IsPollDescriptor(fd uintptr) bool {
	return runtime_isPollServerDescriptor(fd)
}
