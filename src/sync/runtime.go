// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "unsafe"

// defined in package runtime

// Semacquire waits until *s > 0 and then atomically decrements it.
// It is intended as a simple sleep primitive for use by the synchronization
// library and should not be used directly.
func runtime_Semacquire(s *uint32)

// SemacquireMutex is like Semacquire, but for profiling contended Mutexes.
// If lifo is true, queue waiter at the head of wait queue.
// skipframes is the number of frames to omit during tracing, counting from
// runtime_SemacquireMutex's caller.
func runtime_SemacquireMutex(s *uint32, lifo bool, skipframes int)

// Semrelease atomically increments *s and notifies a waiting goroutine
// if one is blocked in Semacquire.
// It is intended as a simple wakeup primitive for use by the synchronization
// library and should not be used directly.
// If handoff is true, pass count directly to the first waiter.
// skipframes is the number of frames to omit during tracing, counting from
// runtime_Semrelease's caller.
// Semrelease自动给信号量计数器s自增1，用于表示释放了1个资源（通常是全局竞争的资源），
// 同时通知阻塞在Semacquire上等待的G协程
// 如果handoff参数是true，则直接把信号量的所有权交给等待队列里的第一个G
// 否则false，就要调度器来决定把信号量的所有权交给哪个G
// skipframes 指示在错误堆栈跟踪中应该跳过的帧数，用于调试和错误报告，通常可以用来过滤掉不相关的调用堆栈帧，
// 使得错误报告更简洁。
func runtime_Semrelease(s *uint32, handoff bool, skipframes int)

// See runtime/sema.go for documentation.
func runtime_notifyListAdd(l *notifyList) uint32

// See runtime/sema.go for documentation.
func runtime_notifyListWait(l *notifyList, t uint32)

// See runtime/sema.go for documentation.
func runtime_notifyListNotifyAll(l *notifyList)

// See runtime/sema.go for documentation.
func runtime_notifyListNotifyOne(l *notifyList)

// Ensure that sync and runtime agree on size of notifyList.
func runtime_notifyListCheck(size uintptr)
func init() {
	var n notifyList
	runtime_notifyListCheck(unsafe.Sizeof(n))
}

// Active spinning runtime support.
// runtime_canSpin reports whether spinning makes sense at the moment.
// 根据当前的自旋次数和系统状态（如处理器数目、线程负载等）来决定是否可以继续自旋，是则返回true
// i是已自旋等待的次数，由调用者维护
// 这个函数通常在实现自旋锁（spin lock）或者其他需要忙等待的同步原语时使用。
// 它有助于优化锁的性能，通过在适当的时候进行自旋等待，避免线程频繁进入和退出睡眠状态，从而减少上下文切换的开销。
// 当返回true则说明可以继续自旋等待，返回false则说明需进入睡眠了
func runtime_canSpin(i int) bool

// runtime_doSpin does active spinning.
func runtime_doSpin()//底层是调用procyield实现自旋，通过执行30次PAUSE指令，该指令是会占用CPU并消耗CPU时间片的

func runtime_nanotime() int64
