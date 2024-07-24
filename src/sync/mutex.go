// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
type Mutex struct {
	state int32 // 0000 0000 0000 0000 0000 0000 0000 0000
	sema  uint32// 0000 0000 0000 0000 0000 0000 0000 0000
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked = 1 << iota // 1 0001 表示已上锁 mutex is locked
	mutexWoken				// 2 0010
	mutexStarving			// 4 0100
	mutexWaiterShift = iota // 3

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	starvationThresholdNs = 1e6//10ms
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	// 快径，获取锁，只能在锁上无等待队列且不处于饥饿模式的情况下才可以快速获取锁
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		// m.state从0变为1，即
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// 获取锁失败，G进入阻塞等待，也即此时锁已被其他G持有，m.state>=1
	// Slow path (outlined so that the fast path can be inlined)
	// 慢径，做成外联的，这样快径的代码就可以内联到调用者里
	m.lockSlow()
}

// TryLock tries to lock m and reports whether it succeeded.
//
// Note that while correct uses of TryLock do exist, they are rare,
// and use of TryLock is often a sign of a deeper problem
// in a particular use of mutexes.
func (m *Mutex) TryLock() bool {
	old := m.state
	if old&(mutexLocked|mutexStarving) != 0 {
		return false
	}

	// There may be a goroutine waiting for the mutex, but we are
	// running now and can try to grab the mutex before that
	// goroutine wakes up.
	if !atomic.CompareAndSwapInt32(&m.state, old, old|mutexLocked) {
		return false
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
	return true
}

func (m *Mutex) lockSlow() {
	var waitStartTime int64
	starving := false//是否饥饿模式
	awoke := false//本G是否已从睡眠模式中唤醒
	iter := 0
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		// 不在饥饿模式下才自旋，饥饿模式下锁所有权是直接交给等待G的，
		// 所以本g（新来尝试获取锁的g）在饥饿模式下是没法获取锁，没必须进行自旋浪费CPU，进入睡眠即可
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// 锁不处于饥饿模式且处于上锁态中 && 可以自旋，则
			// 尝试设置 mutexWoken标志 以通知 Unlock 不要唤醒其他被阻塞的G（为了把锁能优先给新来的G）
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				// 等待队列不为空且未有睡眠中的G被唤醒
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) { // 通过设置锁的唤醒标志位来阻止唤醒等待队列中的G
				awoke = true
			}
			runtime_doSpin()//底层是调用procyield实现自旋，通过执行30次PAUSE指令，该指令是会占用CPU并消耗CPU时间片的
			iter++
			old = m.state//此时，old=1011
			continue
		}

		//因runtime_canSpin(iter)返回false，不可自旋，则走这里逻辑
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		// 在饥饿模式下，新来的G不要尝试获取锁，必须到队尾排队
		if old&mutexStarving == 0 {//当前不是饥饿模式，才可以给刚新来的G获取锁
			new |= mutexLocked
		}
		if old&(mutexLocked|mutexStarving) != 0 {//当前锁处于上锁态或饥饿模式
			new += 1 << mutexWaiterShift// 则新来的G加入到等待队列，并进行睡眠
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		// G被唤醒后，若当前锁依旧处于上锁态，则把锁切换到饥饿模式
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving//锁切换到饥饿模式
		}
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			// G已从睡眠中唤醒，因此重置睡眠位
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken // 因为睡眠的G已被唤醒，所以需清除锁里的 mutexWoken 位标志，即把 new 的右2位 置0
		}
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			if old&(mutexLocked|mutexStarving) == 0 { //没上锁且不处于饥饿，则说明获取到锁了就break退出本函数
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			// 如果本G已经等待过一次了（本次是第n次尝试获取锁），则直接排到等待队列的队头
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				//第一次尝试获取锁
				waitStartTime = runtime_nanotime()//开始等待锁的时间
			}
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)//阻塞挂起，等待锁释放的信号量

			// 恢复运行
			// 先判断下已经等待了多久，超过10ms则把锁切换为饥饿模式
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs//当g等待锁的时间超过10ms，则切换为饥饿模式
			old = m.state
			if old&mutexStarving != 0 {//锁处于饥饿模式
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				if !starving || old>>mutexWaiterShift == 1 {//尝试获取锁的G非饥饿态 或 锁的等待队列上只有1个G时
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					// 锁降级退出饥饿模式
					// 这里降级的做法待争议，饥饿模型很低效，一旦把锁升级为饥饿模式，两个协程G会陷入无限尝试获取锁的死循环里
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				break
			}
			awoke = true
			iter = 0
		} else {
			//还是无法获取到锁，继续等待（自旋or休眠）
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked) // new = m.state - 1，解锁
	//如果m.state的新值即new=0，表示没有其他 goroutine 在等待锁，返回
	if new != 0 {//new>0,说明有其他 goroutine 在等待锁，进入慢路径处理，会在unlockSlow 方法检查等待队列，唤醒等待的 goroutine
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	if (new+mutexLocked)&mutexLocked == 0 {//原持有锁的G还未解锁
		throw("sync: unlock of unlocked mutex")
	}
	if new&mutexStarving == 0 {//锁处于非饥饿模式
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			// 锁上无等待队列 或 锁处于 上锁态|唤醒态|饥饿模式（这意味着没等待该锁的G，也没有新来尝试获取该锁的G了）
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				// old>>mutexWaiterShift 即等待队列为空，无G在等待
				// old&(mutexLocked|mutexWoken|mutexStarving) != 0 即 锁处于 上锁或被唤醒或饥饿态
				return
			}
			// Grab the right to wake someone.
			// 等待队列里还有G，则唤醒一个G，并且把所有权交给唤醒的G
			new = (old - 1<<mutexWaiterShift) | mutexWoken //唤醒一个G，同时减去处于等待队列的G的个数，并把唤醒标志写入
			if atomic.CompareAndSwapInt32(&m.state, old, new) {//把锁的状态写入一个 唤醒标志
				runtime_Semrelease(&m.sema, false, 1)//释放信号量
				return
			}
			old = m.state
		}
	} else {
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		// 锁处于饥饿模式，直接把锁给下一个waiter/G（通过调用semrelease函数时传入handoff=true实现），
		// 是等待队列队头的G
		// 并把当前G的CPU时间片让出以便下一个waiter能立即执行
		// 注意：此时锁还未设为mutexLocked，waiter在唤醒后才会设置，即便如此，因为是处于饥饿模式，此时锁
		// 被认为处于上锁态，所以新来的G无法获取该锁
		runtime_Semrelease(&m.sema, true, 1)
	}
}
