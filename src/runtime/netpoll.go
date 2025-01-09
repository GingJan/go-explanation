// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || darwin || dragonfly || freebsd || (js && wasm) || linux || netbsd || openbsd || solaris || windows

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// Integrated network poller (platform-independent part).
// A particular implementation (epoll/kqueue/port/AIX/Windows)
// must define the following functions:
//
// func netpollinit()
//     Initialize the poller. Only called once.
//
// func netpollopen(fd uintptr, pd *pollDesc) int32
//     Arm edge-triggered notifications for fd. The pd argument is to pass
//     back to netpollready when fd is ready. Return an errno value.
//
// func netpollclose(fd uintptr) int32
//     Disable notifications for fd. Return an errno value.
//
// func netpoll(delta int64) gList
//     Poll the network. If delta < 0, block indefinitely. If delta == 0,
//     poll without blocking. If delta > 0, block for up to delta nanoseconds.
//     Return a list of goroutines built by calling netpollready.
//
// func netpollBreak()
//     Wake up the network poller, assumed to be blocked in netpoll.
//
// func netpollIsPollDescriptor(fd uintptr) bool
//     Reports whether fd is a file descriptor used by the poller.

// Error codes returned by runtime_pollReset and runtime_pollWait.
// These must match the values in internal/poll/fd_poll_runtime.go.
const (
	pollNoError        = 0 // no error
	pollErrClosing     = 1 // descriptor is closed
	pollErrTimeout     = 2 // I/O timeout
	pollErrNotPollable = 3 // general error polling descriptor
)

// pollDesc contains 2 binary semaphores, rg and wg, to park reader and writer
// goroutines respectively. The semaphore can be in the following states:
// pdReady - io readiness notification is pending;
//           a goroutine consumes the notification by changing the state to nil.
// pdWait - a goroutine prepares to park on the semaphore, but not yet parked;
//          the goroutine commits to park by changing the state to G pointer,
//          or, alternatively, concurrent io notification changes the state to pdReady,
//          or, alternatively, concurrent timeout/close changes the state to nil.
// G pointer - the goroutine is blocked on the semaphore;
//             io notification or timeout/close changes the state to pdReady or nil respectively
//             and unparks the goroutine.
// nil - none of the above.
const (
	pdReady uintptr = 1
	pdWait  uintptr = 2
)

const pollBlockSize = 4 * 1024

// Network poller descriptor.
//
// No heap pointers.
//
//go:notinheap
// 网络poller描述符。是Go runtime 用来管理网络 I/O 操作（如 epoll 或 kqueue）的一个结构体
type pollDesc struct {
	link *pollDesc // in pollcache, protected by pollcache.lock
	fd   uintptr   // constant for pollDesc usage lifetime

	// atomicInfo holds bits from closing, rd, and wd,
	// which are only ever written while holding the lock,
	// summarized for use by netpollcheckerr,
	// which cannot acquire the lock.
	// After writing these fields under lock in a way that
	// might change the summary, code must call publishInfo
	// before releasing the lock.
	// Code that changes fields and then calls netpollunblock
	// (while still holding the lock) must call publishInfo
	// before calling netpollunblock, because publishInfo is what
	// stops netpollblock from blocking anew
	// (by changing the result of netpollcheckerr).
	// atomicInfo also holds the eventErr bit,
	// recording whether a poll event on the fd got an error;
	// atomicInfo is the only source of truth for that bit.
	atomicInfo atomic.Uint32 // atomic pollInfo

	// rg, wg are accessed atomically and hold g pointers.
	// (Using atomic.Uintptr here is similar to using guintptr elsewhere.)
	rg atomic.Uintptr // pdReady, pdWait, G waiting for read or nil，取值可能是 pdReady 或 pdWait 或 等待可读的G（的指针） 或 nil
	wg atomic.Uintptr // pdReady, pdWait, G waiting for write or nil，相关的写操作的等待状态，有多个值：nil、pdReady、pdWait或等待写的G（的指针）

	lock    mutex     // 保护下面字段的安全变动
	closing bool      // 当前pollDesc实例是否关闭
	user    uint32    // user settable cookie
	rseq    uintptr   // protects from stale read timers
	rt      timer     // 读超时的计时器 read deadline timer (set if rt.f != nil)
	rd      int64     // 读超时的时长，纳秒，若超时了，该字段值为-1 read deadline (a nanotime in the future, -1 when expired)
	wseq    uintptr   // protects from stale write timers
	wt      timer     // 写超时的计时器 write deadline timer
	wd      int64     // 写超时的时长，纳秒，若超时了，该字段值为-1 write deadline (a nanotime in the future, -1 when expired)
	self    *pollDesc // 指向本实例自己 storage for indirect interface. See (*pollDesc).makeArg.
}

// pollInfo is the bits needed by netpollcheckerr, stored atomically,
// mostly duplicating state that is manipulated under lock in pollDesc.
// The one exception is the pollEventErr bit, which is maintained only
// in the pollInfo.
type pollInfo uint32

const (
	pollClosing = 1 << iota
	pollEventErr
	pollExpiredReadDeadline
	pollExpiredWriteDeadline
)

func (i pollInfo) closing() bool              { return i&pollClosing != 0 }
func (i pollInfo) eventErr() bool             { return i&pollEventErr != 0 }
func (i pollInfo) expiredReadDeadline() bool  { return i&pollExpiredReadDeadline != 0 }
func (i pollInfo) expiredWriteDeadline() bool { return i&pollExpiredWriteDeadline != 0 }

// info returns the pollInfo corresponding to pd.
func (pd *pollDesc) info() pollInfo {
	return pollInfo(pd.atomicInfo.Load())
}

// publishInfo updates pd.atomicInfo (returned by pd.info)
// using the other values in pd.
// It must be called while holding pd.lock,
// and it must be called after changing anything
// that might affect the info bits.
// In practice this means after changing closing
// or changing rd or wd from < 0 to >= 0.
func (pd *pollDesc) publishInfo() {
	var info uint32
	if pd.closing {
		info |= pollClosing
	}
	if pd.rd < 0 {
		info |= pollExpiredReadDeadline
	}
	if pd.wd < 0 {
		info |= pollExpiredWriteDeadline
	}

	// Set all of x except the pollEventErr bit.
	x := pd.atomicInfo.Load()
	for !pd.atomicInfo.CompareAndSwap(x, (x&pollEventErr)|info) {
		x = pd.atomicInfo.Load()
	}
}

// setEventErr sets the result of pd.info().eventErr() to b.
func (pd *pollDesc) setEventErr(b bool) {
	x := pd.atomicInfo.Load()
	for (x&pollEventErr != 0) != b && !pd.atomicInfo.CompareAndSwap(x, x^pollEventErr) {
		x = pd.atomicInfo.Load()
	}
}

type pollCache struct {
	lock  mutex     // 保护下面字段的变动，在alloc，free时都会调用，因为这两个方式会操作first链接
	first *pollDesc // 多个poll对象通过链表方式链接，指向第一个poll对象的地址
	// PollDesc objects must be type-stable,
	// because we can get ready notification from epoll/kqueue
	// after the descriptor is closed/reused.
	// Stale notifications are detected using seq variable,
	// seq is incremented when deadlines are changed or descriptor is reused.
}

var (
	netpollInitLock mutex  //保护netpollInited全局变量
	netpollInited   uint32 //netpoll是否初始化

	pollcache      pollCache //poll复用池
	netpollWaiters uint32    //等待netpoll的协程数量
)

//go:linkname poll_runtime_pollServerInit internal/poll.runtime_pollServerInit
func poll_runtime_pollServerInit() {
	netpollGenericInit()
}

// 初始化netpoll，底层是创建epoll实例
func netpollGenericInit() {
	if atomic.Load(&netpollInited) == 0 {
		lockInit(&netpollInitLock, lockRankNetpollInit) //初始化netpoll初始锁
		lock(&netpollInitLock)                          //对netpool初始化锁 上锁
		if netpollInited == 0 {
			netpollinit() //根据系统不同调用不同的netpollinit进行epoll创建并初始化
			atomic.Store(&netpollInited, 1)
		}
		unlock(&netpollInitLock)
	}
}

//网络轮询是否已初始化
func netpollinited() bool {
	return atomic.Load(&netpollInited) != 0
}

//go:linkname poll_runtime_isPollServerDescriptor internal/poll.runtime_isPollServerDescriptor

// poll_runtime_isPollServerDescriptor reports whether fd is a
// descriptor being used by netpoll.
func poll_runtime_isPollServerDescriptor(fd uintptr) bool {
	return netpollIsPollDescriptor(fd)
}

//底层调用epoll_add(ADD)，添加指定fd的事件监听
// return *pollDesc，这个变量是对fd封装的结构体的实例
// return int，错误码
//go:linkname poll_runtime_pollOpen internal/poll.runtime_pollOpen
func poll_runtime_pollOpen(fd uintptr) (*pollDesc, int) {
	pd := pollcache.alloc() //从复用池里分配一个空闲的pollDesc实例，pd用于对fd封装
	lock(&pd.lock)
	/******对pd状态进行一个检查*******/
	wg := pd.wg.Load()
	if wg != 0 && wg != pdReady { //此时wg为等待写的G 或 nil 或 pdWait=2。意味着此时有一个正在等待写操作的 G，说明该pd有异常（因为从复用池里取出的pd实例理论上是没被使用的才对）
		throw("runtime: blocked write on free polldesc")
	}
	rg := pd.rg.Load()
	if rg != 0 && rg != pdReady { //同上
		throw("runtime: blocked read on free polldesc")
	}
	/******对pd状态进行一个检查*******/

	pd.fd = fd //对fd进行封装
	pd.closing = false
	pd.setEventErr(false)
	pd.rseq++
	pd.rg.Store(0)
	pd.rd = 0
	pd.wseq++
	pd.wg.Store(0)
	pd.wd = 0
	pd.self = pd
	pd.publishInfo()
	unlock(&pd.lock)

	errno := netpollopen(fd, pd) //把fd添加到epoll里，并于pd绑定，当事件触发时，调用pd
	if errno != 0 {              //添加过程出现错误，则
		pollcache.free(pd) //归还pd到poll池
		return nil, int(errno)
	}
	return pd, 0
}

// 把pd指向的底层fd从epoll的监听队列移除，底层调用epoll_add(DEL)，删除指定fd的事件监听
//go:linkname poll_runtime_pollClose internal/poll.runtime_pollClose
func poll_runtime_pollClose(pd *pollDesc) {
	if !pd.closing {
		throw("runtime: close polldesc w/o unblock")
	}
	wg := pd.wg.Load()
	if wg != 0 && wg != pdReady {
		throw("runtime: blocked write on closing polldesc")
	}
	rg := pd.rg.Load()
	if rg != 0 && rg != pdReady {
		throw("runtime: blocked read on closing polldesc")
	}
	netpollclose(pd.fd) //把pd.fd从底层的epoll监听队列里移除
	pollcache.free(pd)  //把pd释放，放回复用池
}

func (c *pollCache) free(pd *pollDesc) {
	lock(&c.lock)
	pd.link = c.first
	c.first = pd
	unlock(&c.lock)
}

// 重置runtime.pollDesc，以便后续复用
// poll_runtime_pollReset, which is internal/poll.runtime_pollReset,
// prepares a descriptor for polling in mode, which is 'r' or 'w'.
// This returns an error code; the codes are defined above.
//go:linkname poll_runtime_pollReset internal/poll.runtime_pollReset
func poll_runtime_pollReset(pd *pollDesc, mode int) int {
	errcode := netpollcheckerr(pd, int32(mode))
	if errcode != pollNoError {
		return errcode
	}
	if mode == 'r' {
		pd.rg.Store(0)
	} else if mode == 'w' {
		pd.wg.Store(0)
	}
	return pollNoError
}

// poll_runtime_pollWait, which is internal/poll.runtime_pollWait,
// waits for a descriptor to be ready for reading or writing,
// according to mode, which is 'r' or 'w'.
// This returns an error code; the codes are defined above.
//go:linkname poll_runtime_pollWait internal/poll.runtime_pollWait
func poll_runtime_pollWait(pd *pollDesc, mode int) int {
	errcode := netpollcheckerr(pd, int32(mode)) //检查底层的fd是否有异常
	if errcode != pollNoError {
		return errcode
	}

	// As for now only Solaris, illumos, and AIX use level-triggered IO.
	if GOOS == "solaris" || GOOS == "illumos" || GOOS == "aix" {
		netpollarm(pd, mode)
	}

	for !netpollblock(pd, int32(mode), false) { // 阻塞在此，直到有事件触发才继续
		errcode = netpollcheckerr(pd, int32(mode))
		if errcode != pollNoError {
			return errcode
		}
		// Can happen if timeout has fired and unblocked us,
		// but before we had a chance to run, timeout has been reset.
		// Pretend it has not happened and retry.
	}
	return pollNoError
}

//go:linkname poll_runtime_pollWaitCanceled internal/poll.runtime_pollWaitCanceled
func poll_runtime_pollWaitCanceled(pd *pollDesc, mode int) {
	// This function is used only on windows after a failed attempt to cancel
	// a pending async IO operation. Wait for ioready, ignore closing or timeouts.
	// 当尝试取消异步IO操作的等待失败后 才调用本函数
	for !netpollblock(pd, int32(mode), true) {
	}
}

//go:linkname poll_runtime_pollSetDeadline internal/poll.runtime_pollSetDeadline
func poll_runtime_pollSetDeadline(pd *pollDesc, d int64, mode int) {
	lock(&pd.lock)
	if pd.closing {
		unlock(&pd.lock)
		return
	}
	rd0, wd0 := pd.rd, pd.wd
	combo0 := rd0 > 0 && rd0 == wd0
	if d > 0 {
		d += nanotime()
		if d <= 0 {
			// If the user has a deadline in the future, but the delay calculation
			// overflows, then set the deadline to the maximum possible value.
			d = 1<<63 - 1
		}
	}
	if mode == 'r' || mode == 'r'+'w' {
		pd.rd = d
	}
	if mode == 'w' || mode == 'r'+'w' {
		pd.wd = d
	}
	pd.publishInfo()
	combo := pd.rd > 0 && pd.rd == pd.wd
	rtf := netpollReadDeadline
	if combo {
		rtf = netpollDeadline
	}
	if pd.rt.f == nil {
		if pd.rd > 0 {
			pd.rt.f = rtf
			// Copy current seq into the timer arg.
			// Timer func will check the seq against current descriptor seq,
			// if they differ the descriptor was reused or timers were reset.
			pd.rt.arg = pd.makeArg()
			pd.rt.seq = pd.rseq
			resettimer(&pd.rt, pd.rd)
		}
	} else if pd.rd != rd0 || combo != combo0 {
		pd.rseq++ // invalidate current timers
		if pd.rd > 0 {
			modtimer(&pd.rt, pd.rd, 0, rtf, pd.makeArg(), pd.rseq)
		} else {
			deltimer(&pd.rt)
			pd.rt.f = nil
		}
	}
	if pd.wt.f == nil {
		if pd.wd > 0 && !combo {
			pd.wt.f = netpollWriteDeadline
			pd.wt.arg = pd.makeArg()
			pd.wt.seq = pd.wseq
			resettimer(&pd.wt, pd.wd)
		}
	} else if pd.wd != wd0 || combo != combo0 {
		pd.wseq++ // invalidate current timers
		if pd.wd > 0 && !combo {
			modtimer(&pd.wt, pd.wd, 0, netpollWriteDeadline, pd.makeArg(), pd.wseq)
		} else {
			deltimer(&pd.wt)
			pd.wt.f = nil
		}
	}
	// If we set the new deadline in the past, unblock currently pending IO if any.
	// Note that pd.publishInfo has already been called, above, immediately after modifying rd and wd.
	var rg, wg *g
	if pd.rd < 0 {
		rg = netpollunblock(pd, 'r', false)
	}
	if pd.wd < 0 {
		wg = netpollunblock(pd, 'w', false)
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 3)
	}
	if wg != nil {
		netpollgoready(wg, 3)
	}
}

//go:linkname poll_runtime_pollUnblock internal/poll.runtime_pollUnblock
func poll_runtime_pollUnblock(pd *pollDesc) {
	lock(&pd.lock) //上锁
	if pd.closing {
		throw("runtime: unblock on closing polldesc")
	}
	pd.closing = true
	pd.rseq++
	pd.wseq++
	var rg, wg *g
	pd.publishInfo()
	rg = netpollunblock(pd, 'r', false) //返回阻塞在该pd上等待read的G
	wg = netpollunblock(pd, 'w', false) //返回阻塞在该pd上等待write的G
	if pd.rt.f != nil {
		deltimer(&pd.rt)
		pd.rt.f = nil
	}
	if pd.wt.f != nil {
		deltimer(&pd.wt)
		pd.wt.f = nil
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 3) //把所有被阻塞的g唤醒进入runnable态
	}
	if wg != nil {
		netpollgoready(wg, 3) //把所有被阻塞的g唤醒进入runnable态
	}
}

// netpollready is called by the platform-specific netpoll function.
// It declares that the fd associated with pd is ready for I/O.
// The toRun argument is used to build a list of goroutines to return
// from netpoll. The mode argument is 'r', 'w', or 'r'+'w' to indicate
// whether the fd is ready for reading or writing or both.
//
// This may run while the world is stopped, so write barriers are not allowed.
//go:nowritebarrier
func netpollready(toRun *gList, pd *pollDesc, mode int32) {
	var rg, wg *g
	if mode == 'r' || mode == 'r'+'w' {
		rg = netpollunblock(pd, 'r', true)
	}
	if mode == 'w' || mode == 'r'+'w' {
		wg = netpollunblock(pd, 'w', true)
	}
	if rg != nil {
		toRun.push(rg)
	}
	if wg != nil {
		toRun.push(wg)
	}
}

// 判断底层fd是否已关闭 或 超时 或 不可pollable
func netpollcheckerr(pd *pollDesc, mode int32) int {
	info := pd.info()
	//判断fd是否已关闭
	if info.closing() { //判断底层的poll是否关闭了
		return pollErrClosing
	}

	//判断是否超时
	if (mode == 'r' && info.expiredReadDeadline()) || (mode == 'w' && info.expiredWriteDeadline()) {
		return pollErrTimeout
	}

	// Report an event scanning error only on a read event.
	// An error on a write event will be captured in a subsequent
	// write call that is able to report a more specific error.
	// 该fd不可阻塞轮询（pollable）
	if mode == 'r' && info.eventErr() {
		return pollErrNotPollable
	}

	//没有错误
	return pollNoError
}

func netpollblockcommit(gp *g, gpp unsafe.Pointer) bool {
	r := atomic.Casuintptr((*uintptr)(gpp), pdWait, uintptr(unsafe.Pointer(gp))) //把gp设为 pdWait 状态
	if r {
		// Bump the count of goroutines waiting for the poller.
		// The scheduler uses this to decide whether to block
		// waiting for the poller if there is nothing else to do.
		atomic.Xadd(&netpollWaiters, 1)
	}
	return r
}

// 把 阻塞在netpoll上等待可读的G（gp）的状态更新为runnable，也即该gp上监听的事件有回应了
func netpollgoready(gp *g, traceskip int) {
	atomic.Xadd(&netpollWaiters, -1)
	goready(gp, traceskip+1) //更新goroutine状态，使得goroutine 进入runnable态
}

// returns true if IO is ready, or false if timedout or closed
// waitio - wait only for completed IO, ignore errors
// Concurrent calls to netpollblock in the same mode are forbidden, as pollDesc
// can hold only a single waiting goroutine for each mode.
// 如果IO就绪（例如数据已准备好进行读取或写入）则返回true，如果IO已超时或已关闭（例如读取或写入时连接被关闭）则返回false
// waitio参数，waitio模式只关心 I/O 操作是否成功完成，而不关心操作过程中可能发生的错误。
// 禁止在同mode下并发调用netpollblock方法（多个同model的 G 不能同时调用该函数），因为同mode下一个 pollDesc 实例只能阻塞等待一个G，
// 所以各mode下只能有一个G在调用 netpollblock，（比如mode=读）
// 如果多个 G 并发调用 netpollblock，它们会争抢同一个 pollDesc，导致不一致的状态或竞争条件。
func netpollblock(pd *pollDesc, mode int32, waitio bool) bool {
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}

	// set the gpp semaphore to pdWait
	for {
		// Consume notification if already ready.
		// 如果pd下的G已经就绪，则返回
		if gpp.CompareAndSwap(pdReady, 0) {
			return true
		}

		// 如果是这种情况，则还未就绪，把pd下的G设为等待
		if gpp.CompareAndSwap(0, pdWait) {
			break
		}

		// Double check that this isn't corrupt; otherwise we'd loop
		// forever.
		if v := gpp.Load(); v != pdReady && v != 0 {
			throw("runtime: double wait")
		}
	}

	// need to recheck error states after setting gpp to pdWait
	// this is necessary because runtime_pollUnblock/runtime_pollSetDeadline/deadlineimpl
	// do the opposite: store to closing/rd/wd, publishInfo, load of rg/wg
	if waitio || netpollcheckerr(pd, mode) == pollNoError {
		//挂起G，至此，当前G让出执行权
		gopark(netpollblockcommit, unsafe.Pointer(gpp), waitReasonIOWait, traceEvGoBlockNet, 5)
	}
	// be careful to not lose concurrent pdReady notification
	old := gpp.Swap(0)
	if old > pdWait {
		throw("runtime: corrupted polldesc")
	}
	return old == pdReady
}

//解除阻塞在io上的G，并返回该G
func netpollunblock(pd *pollDesc, mode int32, ioready bool) *g {
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}

	for {
		old := gpp.Load()
		if old == pdReady {
			return nil
		}
		if old == 0 && !ioready {
			// Only set pdReady for ioready. runtime_pollWait
			// will check for timeout/cancel before waiting.
			return nil
		}
		var new uintptr
		if ioready {
			new = pdReady
		}
		if gpp.CompareAndSwap(old, new) {
			if old == pdWait {
				old = 0
			}
			return (*g)(unsafe.Pointer(old))
		}
	}
}

func netpolldeadlineimpl(pd *pollDesc, seq uintptr, read, write bool) {
	lock(&pd.lock)
	// Seq arg is seq when the timer was set.
	// If it's stale, ignore the timer event.
	currentSeq := pd.rseq
	if !read {
		currentSeq = pd.wseq
	}
	if seq != currentSeq {
		// The descriptor was reused or timers were reset.
		unlock(&pd.lock)
		return
	}
	var rg *g
	if read {
		if pd.rd <= 0 || pd.rt.f == nil {
			throw("runtime: inconsistent read deadline")
		}
		pd.rd = -1 //置为已超时
		pd.publishInfo()
		rg = netpollunblock(pd, 'r', false) //解除阻塞在该pd上等待读的G
	}
	var wg *g
	if write {
		if pd.wd <= 0 || pd.wt.f == nil && !read {
			throw("runtime: inconsistent write deadline")
		}
		pd.wd = -1 //置为已超时
		pd.publishInfo()
		wg = netpollunblock(pd, 'w', false) //解除阻塞在该pd上等待写的G
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 0) //把该G设为runnable等待调度器调度执行
	}
	if wg != nil {
		netpollgoready(wg, 0) //把该G设为runnable等待调度器调度执行
	}
}

func netpollDeadline(arg any, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, true)
}

func netpollReadDeadline(arg any, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, false)
}

func netpollWriteDeadline(arg any, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, false, true)
}

//从poll复用池里取一个poll（从队头弹出）
func (c *pollCache) alloc() *pollDesc {
	lock(&c.lock)
	if c.first == nil { //如果队列链表里没有实例了，则新建一个pollDesc实例，并插入到队头
		const pdSize = unsafe.Sizeof(pollDesc{})
		n := pollBlockSize / pdSize
		if n == 0 {
			n = 1
		}
		// Must be in non-GC memory because can be referenced
		// only from epoll/kqueue internals.
		mem := persistentalloc(n*pdSize, 0, &memstats.other_sys)
		for i := uintptr(0); i < n; i++ {
			pd := (*pollDesc)(add(mem, i*pdSize))
			pd.link = c.first
			c.first = pd
		}
	}
	pd := c.first                        //取出队头的实例
	c.first = pd.link                    //c指向第二个实例
	lockInit(&pd.lock, lockRankPollDesc) //初始化取出的pd实例的lock
	unlock(&c.lock)
	return pd //返回从缓存pollCache里取出的实例
}

// makeArg converts pd to an interface{}.
// makeArg does not do any allocation. Normally, such
// a conversion requires an allocation because pointers to
// go:notinheap types (which pollDesc is) must be stored
// in interfaces indirectly. See issue 42076.
func (pd *pollDesc) makeArg() (i any) {
	x := (*eface)(unsafe.Pointer(&i))
	x._type = pdType
	x.data = unsafe.Pointer(&pd.self)
	return
}

var (
	pdEface any    = (*pollDesc)(nil)
	pdType  *_type = efaceOf(&pdEface)._type
)
