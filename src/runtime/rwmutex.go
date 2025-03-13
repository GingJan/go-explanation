// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
)

// 本文件是 sync/rwmutex.go 的副本，被重写并用于runtime

// rwmutex  是一个读/写互斥锁，该锁可被任意多个reader或一个writer持有
// 该结构体是 sync.RWMutex 用于runtime包的变体， 是 Go 运行时内部实现的读写锁。
// 但它不会与Go调度器交互。与互斥锁 mutex 一样，会阻塞调用方的M（OS线程），而sync.RWMutex阻塞的是G
// 因此该结构体用于runtime，实现需要阻塞OS线程的操作，更适合保护运行时内部的低级资源，如 垃圾回收（GC）、调度器同步、内存管理
type rwmutex struct {
	rLock      mutex    // 用于保护 readers, readerPass, writer 等字段
	readers    muintptr // list of pending readers 当前正在等待读锁的 reader（读操作 goroutine）链表
	readerPass uint32   // number of pending readers to skip readers list

	wLock  mutex    // serializes writers 一个普通的 互斥锁（mutex），它用于控制多个writer的竞争
	writer muintptr // pending writer waiting for completing readers 等待reader释放锁的writer对应的OS线程M

	readerCount uint32 // number of pending readers 当前正在等待读锁的 reader（读操作 goroutine）数量，维护了当前活跃的 reader 数量
	readerWait  uint32 // number of departing readers 当前正在释放读锁的 reader（读操作 goroutine）数量，当该值为0时，writer才能获取到写锁
}

const rwmutexMaxReaders = 1 << 30

// rlock 上读锁
func (rw *rwmutex) rlock() {
	// The reader must not be allowed to lose its P or else other
	// things blocking on the lock may consume all of the Ps and
	// deadlock (issue #20903). Alternatively, we could drop the P
	// while sleeping.
	acquirem()
	//atomic.Xadd(&rw.readerCount, 1) reader持有读锁数量+1
	// < 0 是用于判断当前是否有writer在等待/持有写锁，如果有，则不能再加读锁，要进入队列等待
	if int32(atomic.Xadd(&rw.readerCount, 1)) < 0 {
		// A writer is pending. Park on the reader queue.
		// 有writer在等待，把本次申请读锁的reader放入到队列里等待
		systemstack(func() { //本代码块在系统线程的栈里运行
			lockWithRank(&rw.rLock, lockRankRwmutexR)
			if rw.readerPass > 0 {
				// Writer finished.
				rw.readerPass -= 1
				unlock(&rw.rLock)
			} else {
				// Queue this reader to be woken by
				// the writer.
				// reader放入等待队列，等writer的唤醒
				m := getg().m
				m.schedlink = rw.readers
				rw.readers.set(m)
				unlock(&rw.rLock)
				notesleep(&m.park) //阻塞挂起

				//阻塞解除，恢复运行

				noteclear(&m.park)
			}
		})
	}
}

// runlock 解一次读锁
func (rw *rwmutex) runlock() {
	if r := int32(atomic.Xadd(&rw.readerCount, -1)); r < 0 { //如果 r < 0，说明有writer在等待写锁
		if r+1 == 0 || r+1 == -rwmutexMaxReaders {
			throw("runlock of unlocked rwmutex") //解了未上的读锁
		}
		// A writer is pending.
		if atomic.Xadd(&rw.readerWait, -1) == 0 { //最后一个释放读锁的reader，要唤醒等待写锁的writer
			// The last reader unblocks the writer.
			lockWithRank(&rw.rLock, lockRankRwmutexR)
			w := rw.writer.ptr()
			if w != nil {
				notewakeup(&w.park) //唤醒writer（对应的G）
			}
			unlock(&rw.rLock)
		}
	}
	releasem(getg().m)
}

// lock 上写锁. 等待所有的reader释放读锁后，才能获取写锁
func (rw *rwmutex) lock() {
	// Resolve competition with other writers and stick to our P.
	// 绑定当前处理器 P，减少调度器干扰，提高性能
	lockWithRank(&rw.wLock, lockRankRwmutexW) //先上一个互斥锁，控制多个writer的竞争

	//以下代码只有一个writer可执行（上述持有互斥锁的writer）
	m := getg().m //获取当前m（OS线程）
	// Announce that there is a pending writer.
	// 标记有一个等待写锁的writer
	// 先通过 rw.readerCount -= rwmutexMaxReaders，使得rw.readerCount变为负数，这告知了其他尝试获取读锁的reader知道有一个writer正等待写锁
	// 重新加rwmutexMaxReaders目的是要获取当前有多少个reader持有读锁
	r := int32(atomic.Xadd(&rw.readerCount, -rwmutexMaxReaders)) + rwmutexMaxReaders //有r个reader持有读锁
	// Wait for any active readers to complete.
	// 等待全部reader释放读锁
	lockWithRank(&rw.rLock, lockRankRwmutexR)          // 上一个互斥锁，确保操作 rw.readerWait 的安全
	if r != 0 && atomic.Xadd(&rw.readerWait, r) != 0 { // !=0 说明还有reader未释放锁，writer进入等待
		// Wait for reader to wake us up.
		// writer进入等待，等reader都解锁后通知
		systemstack(func() {
			rw.writer.set(m)
			unlock(&rw.rLock)  //对rw.readerWait的操作完毕，释放锁
			notesleep(&m.park) //挂起当前 goroutine，等待 notewakeup(&m.park) 唤醒（此处是把）

			//当 notewakeup(&m.park) 触发，goroutine 继续执行以下逻辑。

			noteclear(&m.park) // 清除 `note` 状态
		})
	} else {
		//对rw.readerWait 的操作完毕，释放锁
		unlock(&rw.rLock)
	}
}

// unlock 解写锁
func (rw *rwmutex) unlock() {
	// Announce to readers that there is no active writer.
	r := int32(atomic.Xadd(&rw.readerCount, rwmutexMaxReaders))
	if r >= rwmutexMaxReaders {
		throw("unlock of unlocked rwmutex")
	}
	// Unblock blocked readers.
	lockWithRank(&rw.rLock, lockRankRwmutexR)
	for rw.readers.ptr() != nil {
		reader := rw.readers.ptr()
		rw.readers = reader.schedlink
		reader.schedlink.set(nil)
		notewakeup(&reader.park)
		r -= 1
	}
	// If r > 0, there are pending readers that aren't on the
	// queue. Tell them to skip waiting.
	rw.readerPass += uint32(r)
	unlock(&rw.rLock)
	// Allow other writers to proceed.
	unlock(&rw.wLock)
}
