// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package poll

import "sync/atomic"

// fdMutex is a specialized synchronization primitive that manages
// lifetime of an fd and serializes access to Read, Write and Close
// methods on FD.
// fdMutex 是一种专用于fd的同步原语，它用于管理fd的生命周期，以及序列化/串行化对FD的读写关闭等方法的访问
// 因同一个fd可能会被多个协程并发使用，因此需要该结构体来维护对该fd的使用和关闭
type fdMutex struct {
	state uint64 // 以下各位代表的含义： 32-24等待R锁的数量 23-4并发调用数量 3WLock 2RLock 1是否关闭
	rsema uint32 // 读信号量
	wsema uint32 // 读信号量
}

// fdMutex.state is organized as follows:
// 1 bit - whether FD is closed, if set all subsequent lock operations will fail.
// 1 bit - lock for read operations.
// 1 bit - lock for write operations.
// 20 bits - total number of references (read+write+misc).
// 20 bits - number of outstanding read waiters.
// 20 bits - number of outstanding write waiters.
const (
	mutexClosed = 1 << 0 // 0001
	mutexRLock  = 1 << 1 // 0010
	mutexWLock  = 1 << 2 // 0100

	mutexRef     = 1 << 3            // 1000
	mutexRefMask = (1<<20 - 1) << 3  // 0000 0000 0000 0000 0000 0000 0000 0000 0000 0000 0111 1111 1111 1111 1111 1000 /64b
	mutexRWait   = 1 << 23           // 0000 0000 0000 0000 0000 0000 0000 0000 0000 0000 1000 0000 0000 0000 0000 0000
	mutexRMask   = (1<<20 - 1) << 23 // 0000 0000 0000 0000 0000 0000 0000 0000 1111 1111 1000 0000 0000 0000 0000 0000
	mutexWWait   = 1 << 43           // 0000 0000 0000 0000 0000 1000 0000 0000 0000 0000 0000 0000 0000 0000 0000 0000
	mutexWMask   = (1<<20 - 1) << 43 // 0000 0000 0000 1111 1111 1000 0000 0000 0000 0000 0000 0000 0000 0000 0000 0000
)

const overflowMsg = "too many concurrent operations on a single file or socket (max 1048575)"

// 读操作必须调用 rwlock(true)/rwunlock(true) 进行上/解锁
//
// 写操作必须调用 rwlock(false)/rwunlock(false) 进行上/解锁
//
// 其他操作必须调用 incref/decref
// 其他操作即包含如 setsockopt 和 setDeadline等方法
// They need to use incref/decref to ensure that they operate on the
// 因为需要使用 incref/decref 来确保当其他协程仍在使用该fd时（多协程并发使用同一fd），不至于被某个协程突然关闭了该fd
//
// 关闭操作必须调用 increfAndClose/decref.

// incref adds a reference to mu.
// It reports whether mu is available for reading or writing.
// 引用次数自增1，返回是否可以进行读写
func (mu *fdMutex) incref() bool {
	for {
		old := atomic.LoadUint64(&mu.state) //原值
		if old&mutexClosed != 0 {           //当前已是关闭态
			return false
		} //原值不为0，则返回
		new := old + mutexRef
		if new&mutexRefMask == 0 { //在该资源上有太多并发调用了，最多只能有1048575个并发调用
			panic(overflowMsg)
		}
		if atomic.CompareAndSwapUint64(&mu.state, old, new) {
			return true
		}
	}
}

// 增加引用次数，并标记为关闭
func (mu *fdMutex) increfAndClose() bool {
	for {
		old := atomic.LoadUint64(&mu.state) //加载原值
		if old&mutexClosed != 0 {           //已关闭
			return false
		} //原值不为0，则返回
		// Mark as closed and acquire a reference.
		new := (old | mutexClosed) + mutexRef
		if new&mutexRefMask == 0 {
			panic(overflowMsg)
		}
		// Remove all read and write waiters.
		new &^= mutexRMask | mutexWMask
		if atomic.CompareAndSwapUint64(&mu.state, old, new) {
			// Wake all read and write waiters,
			// they will observe closed flag after wakeup.
			for old&mutexRMask != 0 {
				old -= mutexRWait
				runtime_Semrelease(&mu.rsema) //发出信号量，此时mu.rsema自增1
			}
			for old&mutexWMask != 0 {
				old -= mutexWWait
				runtime_Semrelease(&mu.wsema) //发出信号量，此时mu.wsema自增1
			}
			return true
		}
	}
}

// decref removes a reference from mu.
// It reports whether there is no remaining reference.
// 从mu里去掉一次引用，当最后一次引用都被去掉时，说明该FD已无调用方使用了，可以进行关闭了（此时本方法返回true）
func (mu *fdMutex) decref() bool {
	for {
		old := atomic.LoadUint64(&mu.state)
		if old&mutexRefMask == 0 {
			panic("inconsistent poll.fdMutex")
		}
		new := old - mutexRef
		if atomic.CompareAndSwapUint64(&mu.state, old, new) {
			return new&(mutexClosed|mutexRefMask) == mutexClosed
		}
	}
}

// lock adds a reference to mu and locks mu.
// It reports whether mu is available for reading or writing.
// 增加引用次数并且获取读/写锁，返回true表示可以进行读/写了，当锁被占用时，则阻塞等待锁释放
func (mu *fdMutex) rwlock(read bool) bool {
	var mutexBit, mutexWait, mutexMask uint64
	var mutexSema *uint32
	if read {
		mutexBit = mutexRLock
		mutexWait = mutexRWait
		mutexMask = mutexRMask
		mutexSema = &mu.rsema
	} else {
		mutexBit = mutexWLock
		mutexWait = mutexWWait
		mutexMask = mutexWMask
		mutexSema = &mu.wsema
	}
	for {
		old := atomic.LoadUint64(&mu.state)
		if old&mutexClosed != 0 { //当前fd已关闭
			return false
		} // 判断 old 是否 > 0
		var new uint64
		if old&mutexBit == 0 { //RLock 或 WLock是空闲的
			// Lock is free, acquire it.
			// 锁是空闲的，则获取该锁
			new = (old | mutexBit) + mutexRef //(old | mutexBit)是标记为上锁
			if new&mutexRefMask == 0 {        //并发调用的数量太多了
				panic(overflowMsg)
			}
		} else {
			// Wait for lock.
			// 等待锁
			new = old + mutexWait
			if new&mutexMask == 0 { //等待该锁的调用方太多了
				panic(overflowMsg)
			}
		}
		if atomic.CompareAndSwapUint64(&mu.state, old, new) {
			if old&mutexBit == 0 { //如果锁是空闲则，则立即返回
				return true
			} //若该RLock 或 WLock锁当前是空闲的，则本次获取锁成功
			//否则锁不空闲，需等待持有该锁的调用方释放
			runtime_Semacquire(mutexSema) //阻塞在此，等待 mutexSema 信号（当mutexSema值<=0时则阻塞，>0时则立即返回）
			// The signaller has subtracted mutexWait.
		}
	}
}

// unlock removes a reference from mu and unlocks mu.
// It reports whether there is no remaining reference.
func (mu *fdMutex) rwunlock(read bool) bool {
	var mutexBit, mutexWait, mutexMask uint64
	var mutexSema *uint32
	if read {
		mutexBit = mutexRLock
		mutexWait = mutexRWait
		mutexMask = mutexRMask
		mutexSema = &mu.rsema
	} else {
		mutexBit = mutexWLock
		mutexWait = mutexWWait
		mutexMask = mutexWMask
		mutexSema = &mu.wsema
	}
	for {
		old := atomic.LoadUint64(&mu.state)
		if old&mutexBit == 0 || old&mutexRefMask == 0 {
			panic("inconsistent poll.fdMutex")
		}
		// Drop lock, drop reference and wake read waiter if present.
		new := (old &^ mutexBit) - mutexRef
		if old&mutexMask != 0 {
			new -= mutexWait
		}
		if atomic.CompareAndSwapUint64(&mu.state, old, new) {
			if old&mutexMask != 0 {
				runtime_Semrelease(mutexSema)
			}
			return new&(mutexClosed|mutexRefMask) == mutexClosed
		}
	}
}

// Implemented in runtime package.
func runtime_Semacquire(sema *uint32) //获取信号，此时sema自减1，当sema自减1后小于0，那么调用方将会被阻塞
func runtime_Semrelease(sema *uint32) //释放信号，此时sema自增1

// incref adds a reference to fd.
// It returns an error when fd cannot be used.
// incref 往fd上添加一个引用，当fd无法使用时返回错误
func (fd *FD) incref() error {
	if !fd.fdmu.incref() { //若返回false，则fd已关闭了
		return errClosing(fd.isFile) //返回对应的错误码（fd已关闭但再次使用，则错误）
	}
	return nil
}

// 本方法用于记录对FD实例的引用次数。当引用次数为0时，则说明无调用方使用本FD实例了，则可以进行关闭了
func (fd *FD) decref() error {
	if fd.fdmu.decref() { //如果返回true，说明该FD实例已无引用，可以关闭了
		return fd.destroy() //异常epoll监听并关闭底层fd
	} //当最后一个引用都被关闭时（也即无调用方使用本FD实例了），则调用destroy方法关闭底层fd
	return nil
}

// readLock adds a reference to fd and locks fd for reading.
// It returns an error when fd cannot be used for reading.
// 增加引用次数并获取读锁，当锁已被占用则阻塞等待
func (fd *FD) readLock() error {
	if !fd.fdmu.rwlock(true) { //获取读锁
		return errClosing(fd.isFile)
	}
	return nil
}

// readUnlock removes a reference from fd and unlocks fd for reading.
// It also closes fd when the state of fd is set to closed and there
// is no remaining reference.
func (fd *FD) readUnlock() {
	if fd.fdmu.rwunlock(true) { //解读锁
		fd.destroy()
	}
}

// writeLock adds a reference to fd and locks fd for writing.
// It returns an error when fd cannot be used for writing.
// writeLock
func (fd *FD) writeLock() error {
	if !fd.fdmu.rwlock(false) { //获取写锁
		return errClosing(fd.isFile)
	}
	return nil
}

// writeUnlock removes a reference from fd and unlocks fd for writing.
// It also closes fd when the state of fd is set to closed and there
// is no remaining reference.
func (fd *FD) writeUnlock() {
	if fd.fdmu.rwunlock(false) {
		fd.destroy()
	}
}
