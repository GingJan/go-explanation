// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux

package runtime

/*
_EPOLLIN       = 可读事件
_EPOLLOUT      = 可写事件
_EPOLLERR      = 错误事件
_EPOLLHUP      = 挂起事件
_EPOLLRDHUP    = 对端关闭事件
_EPOLLET       = 边缘触发模式，如果要设置水平触发模式，则不添加这个标识即可
_EPOLL_CLOEXEC = 文件描述符的 close-on-exec 标志
_EPOLL_CTL_ADD = 0x1
_EPOLL_CTL_DEL = 0x2
_EPOLL_CTL_MOD = 0x3
*/

func epollcreate(size int32) int32
func epollcreate1(flags int32) int32

//go:noescape
func epollctl(epfd, op, fd int32, ev *epollevent) int32

//epfd是epoll实例的fd，ev则是用于存放就绪事件及其数据的缓冲区，nev是本次调用最多返回多少个事件，timeout则是超时时长
//如果返回值>0，表示有多少个事件发生，并且 ev 中会包含这些事件的信息。
//如果返回值=0，表示超时，即在指定的超时时间内没有事件发生。
//如果返回值<0，表示发生了错误，具体的错误码可以通过 errno 来查看。
//go:noescape
func epollwait(epfd int32, ev *epollevent, nev, timeout int32) int32
func closeonexec(fd int32)

var (
	epfd int32 = -1 // epoll的描述符

	netpollBreakRd, netpollBreakWr uintptr // 给 netpollBreak 函数使用，用于中断epoll的阻塞（中断epollwait）

	netpollWakeSig uint32 // 用于防止重复调用 netpollBreak，当为1时说明有操作把netpoll的阻塞等待唤醒了 used to avoid duplicate calls of netpollBreak
)

// epoll初始化
// 1.创建epoll实例
// 2.创建非阻塞读写管道pipe（用于终端epoll阻塞）
// 3.epoll添加对管道读事件的监听
func netpollinit() {
	epfd = epollcreate1(_EPOLL_CLOEXEC) //创建一个 epoll文件描述
	if epfd < 0 {
		epfd = epollcreate(1024)
		if epfd < 0 {
			println("runtime: epollcreate failed with", -epfd)
			throw("runtime: netpollinit failed")
		}
		closeonexec(epfd)
	}
	r, w, errno := nonblockingPipe() //创建一个非阻塞的读写管道
	if errno != 0 {
		println("runtime: pipe failed with", -errno)
		throw("runtime: pipe failed")
	}
	ev := epollevent{
		events: _EPOLLIN,
	}
	*(**uintptr)(unsafe.Pointer(&ev.data)) = &netpollBreakRd //epoll添加管道的读端的监听（以便后面可通过往netpollBreakWr写入数据来唤醒epoll_wait）
	errno = epollctl(epfd, _EPOLL_CTL_ADD, r, &ev)           //添加对管道读事件的监听
	if errno != 0 {
		println("runtime: epollctl failed with", -errno)
		throw("runtime: epollctl failed")
	}
	netpollBreakRd = uintptr(r)
	netpollBreakWr = uintptr(w)
}

// 判断fd是否 poll fd
func netpollIsPollDescriptor(fd uintptr) bool {
	return fd == uintptr(epfd) || fd == netpollBreakRd || fd == netpollBreakWr
}

func netpollopen(fd uintptr, pd *pollDesc) int32 {
	var ev epollevent
	ev.events = _EPOLLIN | _EPOLLOUT | _EPOLLRDHUP | _EPOLLET // _EPOLLET边缘触发模式
	*(**pollDesc)(unsafe.Pointer(&ev.data)) = pd
	return -epollctl(epfd, _EPOLL_CTL_ADD, int32(fd), &ev) //调用 epollctl 函数，将指定的fd添加到 epoll 实例中进行监听。返回错误码（错误码是负数，所以使用-，负负得正）
}

//释放listen_fd
func netpollclose(fd uintptr) int32 {
	var ev epollevent
	return -epollctl(epfd, _EPOLL_CTL_DEL, int32(fd), &ev)
}

func netpollarm(pd *pollDesc, mode int) {
	throw("runtime: unused")
}

// netpollBreak 中断epollwait
// 通过向netpollBreakWr写入数据来触发另一头netpollBreakRd可读方式来中断epoll_wait
func netpollBreak() {
	if atomic.Cas(&netpollWakeSig, 0, 1) { //如果netpollWakeSig!=0，则说明netpoll已经被唤醒过一次了，则不需要重复唤醒它
		//通过给netpollBreakWr fd写入「数据」的方式，使得监听netpollBreakWr fd的epoll触发可读事件
		for {
			var b byte
			n := write(netpollBreakWr, unsafe.Pointer(&b), 1) //因为写入的数据是1个字节，所以返回的值=1时才正常
			if n == 1 {
				break
			}
			if n == -_EINTR { //继续轮询
				continue
			}
			if n == -_EAGAIN { //非阻塞
				return
			}
			println("runtime: netpollBreak write failed with", -n)
			throw("runtime: netpollBreak write failed")
		}
	}
}

// netpoll checks for ready network connections.
// Returns list of goroutines that become runnable.
// delay < 0: blocks indefinitely
// delay == 0: does not block, just polls
// delay > 0: block for up to that many nanoseconds
// delay < 0: 无限期阻塞等待
// delay == 0: 不等待，调用后立即返回
// delay > 0: 阻塞等待delay纳秒
// 本函数用于阻塞等待网络连接读写就绪，底层调用epollwait，当就绪时，返回runnable的G列表（这些G当初都是被阻塞等待IO）
func netpoll(delay int64) gList {
	if epfd == -1 {
		return gList{}
	}
	var waitms int32 //等待waitms毫秒
	if delay < 0 {
		waitms = -1
	} else if delay == 0 {
		waitms = 0
	} else if delay < 1e6 {
		waitms = 1
	} else if delay < 1e15 {
		waitms = int32(delay / 1e6)
	} else {
		// An arbitrary cap on how long to wait for a timer.
		// 1e9 ms == ~11.5 days.
		waitms = 1e9
	}
	var events [128]epollevent
retry:
	n := epollwait(epfd, &events[0], int32(len(events)), waitms) //等待waitms毫秒，最多返回len(events)个就绪事件，并把事件对应的数据写入到&events[0]指向的缓冲区，当调用netpollBreak往netpollBreakWr写入数据时，netpollBreakRd则有可读事件
	if n < 0 {                                                   //发生了错误
		if n != -_EINTR {
			println("runtime: epollwait on fd", epfd, "failed with", -n)
			throw("runtime: netpoll failed")
		}
		// If a timed sleep was interrupted, just return to
		// recalculate how long we should sleep now.
		if waitms > 0 {
			return gList{}
		}
		goto retry
	}
	var toRun gList
	for i := int32(0); i < n; i++ {
		ev := &events[i] //取出每个事件的数据
		if ev.events == 0 {
			continue
		}

		if *(**uintptr)(unsafe.Pointer(&ev.data)) == &netpollBreakRd {
			if ev.events != _EPOLLIN { //不是可读事件，则是异常情况
				println("runtime: netpoll: break fd ready for", ev.events)
				throw("runtime: netpoll: break fd ready for something unexpected")
			}
			if delay != 0 { //带有超时的pollwait
				// netpollBreak could be picked up by a
				// nonblocking poll. Only read the byte
				// if blocking.
				var tmp [16]byte
				read(int32(netpollBreakRd), noescape(unsafe.Pointer(&tmp[0])), int32(len(tmp)))
				atomic.Store(&netpollWakeSig, 0)
			}
			continue
		}

		var mode int32
		if ev.events&(_EPOLLIN|_EPOLLRDHUP|_EPOLLHUP|_EPOLLERR) != 0 { //如果是这几种事件其中一个，则mode=r
			mode += 'r'
		}
		if ev.events&(_EPOLLOUT|_EPOLLHUP|_EPOLLERR) != 0 { //如果是这几种事件其中一个，则mode=w
			mode += 'w'
		}
		if mode != 0 { //mode是r或w或rw情况下，则
			pd := *(**pollDesc)(unsafe.Pointer(&ev.data)) //ev里的data其实就是 runtime.pollDesc
			pd.setEventErr(ev.events == _EPOLLERR)
			netpollready(&toRun, pd, mode) //把pd下关联的G解除阻塞等待，并把G添加到toRun里
		}
	}
	return toRun //把所有解除阻塞等待的G返回（以便后续处理，如恢复G的运行等）
}
