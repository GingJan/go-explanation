// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || darwin || netbsd || openbsd || plan9 || solaris || windows

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// This implementation depends on OS-specific implementations of
//
//	func semacreate(mp *m)
//		Create a semaphore for mp, if it does not already have one.
//
//	func semasleep(ns int64) int32
//		If ns < 0, acquire m's semaphore and return 0.
//		If ns >= 0, try to acquire m's semaphore for at most ns nanoseconds.
//		Return 0 if the semaphore was acquired, -1 if interrupted or timed out.
//
//	func semawakeup(mp *m)
//		Wake up mp, which is or will soon be sleeping on its semaphore.
//
const (
	locked uintptr = 1

	active_spin     = 4
	active_spin_cnt = 30
	passive_spin    = 1
)

func lock(l *mutex) {
	lockWithRank(l, getLockRank(l))
}
//通过对mutex.key字段的原子操作来获取锁
func lock2(l *mutex) {
	gp := getg()
	if gp.m.locks < 0 {
		throw("runtime·lock: lock count")
	}
	gp.m.locks++

	// Speculative grab for lock.
	if atomic.Casuintptr(&l.key, 0, locked) {
		return
	}
	semacreate(gp.m)//对m所在线程初始化线程互斥锁和线程信号量

	// On uniprocessor's, no point spinning.
	// On multiprocessors, spin for ACTIVE_SPIN attempts.
	spin := 0//自旋次数/尝试次数
	if ncpu > 1 {
		spin = active_spin
	}
Loop:
	for i := 0; ; i++ {
		v := atomic.Loaduintptr(&l.key)
		if v&locked == 0 {//未上锁，尝试获取锁
			// Unlocked. Try to lock.
			if atomic.Casuintptr(&l.key, v, v|locked) {
				return//获取到锁了，立刻返回
			}
			i = 0
		}
		if i < spin {
			procyield(active_spin_cnt)//执行30次PAUSE指令，该指令是会占用CPU并消耗CPU时间片的
		} else if i < spin+passive_spin {
			osyield()//挂起当前线程1us
		} else {
			// Someone else has it.
			// l->waitm points to a linked list of M's waiting
			// for this lock, chained through m->nextwaitm.
			// Queue this M.
			// 其他m持有着该l锁，l.waitm指向等待该锁的m的链表，通过m.nextwaitm链接起来
			// 把本m加入链表里
			for {
				gp.m.nextwaitm = muintptr(v &^ locked)
				if atomic.Casuintptr(&l.key, v, uintptr(unsafe.Pointer(gp.m))|locked) {
					break
				}
				v = atomic.Loaduintptr(&l.key)
				if v&locked == 0 {//解锁了，再次尝试获取锁
					continue Loop
				}
			}
			if v&locked != 0 {//还没解锁
				// Queued. Wait. 则把本线程进入阻塞队列，等待被其他线程唤醒
				semasleep(-1)//把当前线程挂起休眠，直到有其他线程调用semawakeup唤醒
				i = 0
			}
		}
	}
}

func unlock(l *mutex) {
	unlockWithRank(l)
}

//go:nowritebarrier
// We might not be holding a p in this code.
func unlock2(l *mutex) {
	gp := getg()
	var mp *m
	for {
		v := atomic.Loaduintptr(&l.key)
		if v == locked {
			if atomic.Casuintptr(&l.key, locked, 0) {
				break
			}
		} else {
			// Other M's are waiting for the lock.
			// Dequeue an M.
			mp = muintptr(v &^ locked).ptr()
			if atomic.Casuintptr(&l.key, v, uintptr(mp.nextwaitm)) {
				// Dequeued an M.  Wake it.
				semawakeup(mp)
				break
			}
		}
	}
	gp.m.locks--
	if gp.m.locks < 0 {
		throw("runtime·unlock: lock count")
	}
	if gp.m.locks == 0 && gp.preempt { // restore the preemption request in case we've cleared it in newstack
		gp.stackguard0 = stackPreempt
	}
}

// One-time notifications.
func noteclear(n *note) {
	if GOOS == "aix" {
		// On AIX, semaphores might not synchronize the memory in some
		// rare cases. See issue #30189.
		atomic.Storeuintptr(&n.key, 0)
	} else {
		n.key = 0
	}
}

func notewakeup(n *note) {
	var v uintptr
	for {
		v = atomic.Loaduintptr(&n.key)
		if atomic.Casuintptr(&n.key, v, locked) {
			break
		}
	}

	// Successfully set waitm to locked.
	// What was it before?
	switch {
	case v == 0:
		//若原n.key==0，则无任何人在此n上等待
		// Nothing was waiting. Done.
	case v == locked:
		// 若原n.key==locke，则等待在该n的人被唤醒两次，这是不允许的
		// Two notewakeups! Not allowed.
		throw("notewakeup - double wakeup")
	default://原n.key==某个在该n上等待的m
		// Must be the waiting m. Wake it up.
		semawakeup((*m)(unsafe.Pointer(v)))
	}
}

func notesleep(n *note) {
	gp := getg()
	if gp != gp.m.g0 {//当前函数必须在g0上执行
		throw("notesleep not on g0")
	}
	semacreate(gp.m)
	if !atomic.Casuintptr(&n.key, 0, uintptr(unsafe.Pointer(gp.m))) {
		// Must be locked (got wakeup).
		if n.key != locked {
			throw("notesleep - waitm out of sync")
		}
		return
	}
	// Queued. Sleep.
	gp.m.blocked = true//入队，进入休眠
	if *cgo_yield == nil {
		semasleep(-1)//挂起当前线程，进入休眠
	} else {
		// Sleep for an arbitrary-but-moderate interval to poll libc interceptors.
		const ns = 10e6
		for atomic.Loaduintptr(&n.key) == 0 {
			semasleep(ns)//挂起当前线程，进入ns纳秒休眠
			asmcgocall(*cgo_yield, nil)
		}
	}
	gp.m.blocked = false//推出休眠
}

//go:nosplit
func notetsleep_internal(n *note, ns int64, gp *g, deadline int64) bool {
	// gp and deadline are logically local variables, but they are written
	// as parameters so that the stack space they require is charged
	// to the caller.
	// This reduces the nosplit footprint of notetsleep_internal.
	gp = getg()

	// Register for wakeup on n->waitm.
	if !atomic.Casuintptr(&n.key, 0, uintptr(unsafe.Pointer(gp.m))) {
		// Must be locked (got wakeup).
		if n.key != locked {
			throw("notetsleep - waitm out of sync")
		}
		return true
	}
	if ns < 0 {
		// Queued. Sleep.
		gp.m.blocked = true
		if *cgo_yield == nil {
			semasleep(-1)
		} else {
			// Sleep in arbitrary-but-moderate intervals to poll libc interceptors.
			const ns = 10e6
			for semasleep(ns) < 0 {
				asmcgocall(*cgo_yield, nil)
			}
		}
		gp.m.blocked = false
		return true
	}

	deadline = nanotime() + ns
	for {
		// Registered. Sleep.
		gp.m.blocked = true
		if *cgo_yield != nil && ns > 10e6 {
			ns = 10e6
		}
		if semasleep(ns) >= 0 {
			gp.m.blocked = false
			// Acquired semaphore, semawakeup unregistered us.
			// Done.
			return true
		}
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		gp.m.blocked = false
		// Interrupted or timed out. Still registered. Semaphore not acquired.
		ns = deadline - nanotime()
		if ns <= 0 {
			break
		}
		// Deadline hasn't arrived. Keep sleeping.
	}

	// Deadline arrived. Still registered. Semaphore not acquired.
	// Want to give up and return, but have to unregister first,
	// so that any notewakeup racing with the return does not
	// try to grant us the semaphore when we don't expect it.
	for {
		v := atomic.Loaduintptr(&n.key)
		switch v {
		case uintptr(unsafe.Pointer(gp.m)):
			// No wakeup yet; unregister if possible.
			if atomic.Casuintptr(&n.key, v, 0) {
				return false
			}
		case locked:
			// Wakeup happened so semaphore is available.
			// Grab it to avoid getting out of sync.
			gp.m.blocked = true
			if semasleep(-1) < 0 {
				throw("runtime: unable to acquire - semaphore out of sync")
			}
			gp.m.blocked = false
			return true
		default:
			throw("runtime: unexpected waitm - semaphore out of sync")
		}
	}
}

func notetsleep(n *note, ns int64) bool {
	gp := getg()
	if gp != gp.m.g0 {
		throw("notetsleep not on g0")
	}
	semacreate(gp.m)
	return notetsleep_internal(n, ns, nil, 0)
}

// same as runtime·notetsleep, but called on user g (not g0)
// calls only nosplit functions between entersyscallblock/exitsyscall
func notetsleepg(n *note, ns int64) bool {
	gp := getg()
	if gp == gp.m.g0 {
		throw("notetsleepg on g0")
	}
	semacreate(gp.m)
	entersyscallblock()
	ok := notetsleep_internal(n, ns, nil, 0)
	exitsyscall()
	return ok
}

func beforeIdle(int64, int64) (*g, bool) {
	return nil, false
}

func checkTimeouts() {}
