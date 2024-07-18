// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package poll

import "sync/atomic"

// fdMutex is a specialized synchronization primitive that manages
// lifetime of an fd and serializes access to Read, Write and Close
// methods on FD.
// fdMutex 是一种专用于fd的同步原语，它用于管理fd的生命周期，以及序列化/串行化对FD的读写关闭等方法的访问
type fdMutex struct {
	state uint64
	rsema uint32//信号量
	wsema uint32
}

// fdMutex.state is organized as follows:
// 1 bit - whether FD is closed, if set all subsequent lock operations will fail.
// 1 bit - lock for read operations.
// 1 bit - lock for write operations.
// 20 bits - total number of references (read+write+misc).
// 20 bits - number of outstanding read waiters.
// 20 bits - number of outstanding write waiters.
const (
	mutexClosed  = 1 << 0 // 0001
	mutexRLock   = 1 << 1 // 0010
	mutexWLock   = 1 << 2 // 0100

	mutexRef     = 1 << 3 // 1000
	mutexRefMask = (1<<20 - 1) << 3 // 0000 0000 0000 0000 0000 0000 0000 0000 0000 0000 0111 1111 1111 1111 1111 1000 /64b
	mutexRWait   = 1 << 23			// 0000 0000 0000 0000 0000 0000 0000 0000 0000 0000 1000 0000 0000 0000 0000 0000
	mutexRMask   = (1<<20 - 1) << 23// 0000 0000 0000 0000 0000 0000 0000 0000 1111 1111 1000 0000 0000 0000 0000 0000
	mutexWWait   = 1 << 43			// 0000 0000 0000 0000 0000 1000 0000 0000 0000 0000 0000 0000 0000 0000 0000 0000
	mutexWMask   = (1<<20 - 1) << 43// 0000 0000 0000 1111 1111 1000 0000 0000 0000 0000 0000 0000 0000 0000 0000 0000
)

const overflowMsg = "too many concurrent operations on a single file or socket (max 1048575)"

// Read operations must do rwlock(true)/rwunlock(true).
//
// Write operations must do rwlock(false)/rwunlock(false).
//
// Misc operations must do incref/decref.
// Misc operations include functions like setsockopt and setDeadline.
// They need to use incref/decref to ensure that they operate on the
// correct fd in presence of a concurrent close call (otherwise fd can
// be closed under their feet).
//
// Close operations must do increfAndClose/decref.

// incref adds a reference to mu.
// It reports whether mu is available for reading or writing.
// 引用次数自增1，返回是否可以进行读写
func (mu *fdMutex) incref() bool {
	for {
		old := atomic.LoadUint64(&mu.state)
		if old&mutexClosed != 0 {//当前已是关闭态
			return false
		}
		new := old + mutexRef
		if new&mutexRefMask == 0 {
			panic(overflowMsg)
		}
		if atomic.CompareAndSwapUint64(&mu.state, old, new) {
			return true
		}
	}
}

// increfAndClose 关闭mu，如果已关闭则返回false
func (mu *fdMutex) increfAndClose() bool {
	for {
		old := atomic.LoadUint64(&mu.state)
		if old&mutexClosed != 0 {//已关闭
			return false
		}
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
				runtime_Semrelease(&mu.rsema)//发出信号量，此时mu.rsema自增1
			}
			for old&mutexWMask != 0 {
				old -= mutexWWait
				runtime_Semrelease(&mu.wsema)//发出信号量，此时mu.wsema自增1
			}
			return true
		}
	}
}

// decref removes a reference from mu.
// It reports whether there is no remaining reference.
// 从mu里去掉一次引用，返回是否已再无引用
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
// 引用次数自增并且锁上mu，返回是否可以进行读或写了
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
		if old&mutexClosed != 0 {//当前fd已关闭
			return false
		}
		var new uint64
		if old&mutexBit == 0 {
			// Lock is free, acquire it.
			// 锁是空闲的
			new = (old | mutexBit) + mutexRef
			if new&mutexRefMask == 0 {
				panic(overflowMsg)
			}
		} else {
			// Wait for lock.
			// 等待锁
			new = old + mutexWait
			if new&mutexMask == 0 {
				panic(overflowMsg)
			}
		}
		if atomic.CompareAndSwapUint64(&mu.state, old, new) {
			if old&mutexBit == 0 {//如果锁是空闲则，则立即返回
				return true
			}
			//否则锁不空闲
			runtime_Semacquire(mutexSema)//阻塞在此 等待mutexSema信号（当mutexSema值<=0时则阻塞，>0时则立即返回）
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
func runtime_Semacquire(sema *uint32)//获取信号，此时sema自减1，当sema自减1后小于0，那么调用方将会被阻塞
func runtime_Semrelease(sema *uint32)//释放信号，此时sema自增1

// incref adds a reference to fd.
// It returns an error when fd cannot be used.
// incref 往fd上添加一个引用，当fd无法使用时返回错误
func (fd *FD) incref() error {
	if !fd.fdmu.incref() {//返回false，代表该fd已关闭了
		return errClosing(fd.isFile)
	}
	return nil
}

// decref removes a reference from fd.
// It also closes fd when the state of fd is set to closed and there
// is no remaining reference.
func (fd *FD) decref() error {
	if fd.fdmu.decref() {//如果为true，说明该fd已无引用，可以关闭了
		return fd.destroy()//关闭底层fd
	}
	return nil
}

// readLock adds a reference to fd and locks fd for reading.
// It returns an error when fd cannot be used for reading.
func (fd *FD) readLock() error {
	if !fd.fdmu.rwlock(true) {
		return errClosing(fd.isFile)
	}
	return nil
}

// readUnlock removes a reference from fd and unlocks fd for reading.
// It also closes fd when the state of fd is set to closed and there
// is no remaining reference.
func (fd *FD) readUnlock() {
	if fd.fdmu.rwunlock(true) {
		fd.destroy()
	}
}

// writeLock adds a reference to fd and locks fd for writing.
// It returns an error when fd cannot be used for writing.
func (fd *FD) writeLock() error {
	if !fd.fdmu.rwlock(false) {
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
