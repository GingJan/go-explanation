// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || darwin || dragonfly || freebsd || (js && wasm) || linux || netbsd || openbsd || solaris

package poll

import (
	"internal/syscall/unix"
	"io"
	"sync/atomic"
	"syscall"
)

// FD is a file descriptor. The net and os packages use this type as a
// field of a larger type representing a network connection or OS file.
// FD 是底层fd的封装。net和os包使用本结构体来表示网络连接或系统文件（的抽象）
type FD struct {
	// Lock sysfd and serialize access to Read and Write methods.
	// sysfd锁，用于序列化访问 Read 和 Write 方法
	fdmu fdMutex //该字段用于保护底层fd的并发安全（因为本FD结构体可能会被多个协程并发使用），并用于记录有多少个调用方使用本FD实例

	// 系统fd，不可篡改，只会在 Close 关闭时变为-1
	Sysfd int

	// I/O poller.
	pd pollDesc //是 poll.pollDesc，poll.pollDesc 的 runtimeCtx 指向 runtime.pollDesc，runtime.pollDesc 再指向更底层的fd

	// Writev cache.
	iovecs *[]syscall.Iovec

	// Semaphore signaled when file is closed.
	csema uint32 //当文件被关闭时，使用该信号量进行通知

	// Non-zero if this file has been set to blocking mode.
	// 标记该FD实例是否阻塞模式。如果FD被设为阻塞模式，则该值为非0（即1）
	isBlocking uint32

	// Whether this is a streaming descriptor, as opposed to a
	// packet-based descriptor like a UDP socket. Immutable.
	// 流描述符标识，true则为流描述符，false则为类似UDP包的包描述符（也即true则是TCP、false则是UDP）
	IsStream bool

	// Whether a zero byte read indicates EOF. This is false for a
	// message based socket connection.
	// 标识是否EOF，如果是socket连接，则为false
	ZeroReadIsEOF bool

	// Whether this is a file rather than a network socket.
	// 是文件还是网络socket
	isFile bool
}

// 初始化FD实例，在调用本方法前，FD.Sysfd 字段应该已存有值。本方法可以同一个FD实例上调用多次
// net参数的值可以是net包里的network name（如tcp）或 file
// 如果底层fd是由runtime的netpoll管理（底层epoll监听），则pollable参数传入true
func (fd *FD) Init(net string, pollable bool) error {
	// We don't actually care about the various network types.
	if net == "file" {
		fd.isFile = true
	}
	if !pollable { //非pollable，也即只能是阻塞模式
		fd.isBlocking = 1 //不可pollable，初始化时默认设置为阻塞模式
		return nil
	}
	err := fd.pd.init(fd) //初始化底层runtime.pollDesc并创建&初始化epoll实例，然后把fd添加到底层epoll的监听队列里
	if err != nil {
		// If we could not initialize the runtime poller,
		// assume we are using blocking mode.
		// 出现异常，无法初始化runtime.pollDesc，也即无法完成epoll实例的创建，所以先假设使用阻塞模式
		fd.isBlocking = 1 //创建epoll实例时出现异常，先假设使用阻塞模式
	}
	return err
}

// 本方法的行为，从epoll里移除监听并关闭底层fd，当本FD实例不再有引用时（无调用方使用了）才能调用本方法
// 当本FD实例的引用次数为0时，都会调用本方法进行关闭（这么做是为了节约资源考虑，不再使用了则能关闭就关闭）
func (fd *FD) destroy() error {
	// Poller may want to unregister fd in readiness notification mechanism,
	// so this must be executed before CloseFunc.
	// Poller要先删掉fd的注册，所以必须在 CloseFunc 前调用
	fd.pd.close()

	// We don't use ignoringEINTR here because POSIX does not define
	// whether the descriptor is closed if close returns EINTR.
	// If the descriptor is indeed closed, using a loop would race
	// with some other goroutine opening a new descriptor.
	// (The Linux kernel guarantees that it is closed on an EINTR error.)
	// 调用系统方法关闭底层的系统fd（注意在关闭系统fd前，需要在epoll上把该fd的监听注销掉）
	err := CloseFunc(fd.Sysfd)

	fd.Sysfd = -1                 //在destroy时，把sysfd重置为-1
	runtime_Semrelease(&fd.csema) //系统fd被关闭，发出信号
	return err
}

// 关闭该FD实例，当底层fd无引用时，则会被destroy方法关闭
// 当本方法被调用时，则先唤醒所有阻塞在本FD上的等待读写事件的G，然后再把底层fd从epoll监听里移除，然后再关闭底层fd
func (fd *FD) Close() error {
	if !fd.fdmu.increfAndClose() {
		return errClosing(fd.isFile)
	}

	// Unblock any I/O.  Once it all unblocks and returns,
	// so that it cannot be referring to fd.sysfd anymore,
	// the final decref will close fd.sysfd. This should happen
	// fairly quickly, since all the I/O is non-blocking, and any
	// attempts to block in the pollDesc will return errClosing(fd.isFile).
	// 接触任何IO阻塞，
	fd.pd.evict() //唤醒所有阻塞在该fd上的G，这里只是解除阻塞，而不会把fd从epoll里移除监听

	// The call to decref will call destroy if there are no other
	// references.
	err := fd.decref() //如果该FD实例已无其他协程调用，则会关闭底层的fd（关闭fd并从epoll上异常监听）

	// Wait until the descriptor is closed. If this was the only
	// reference, it is already closed. Only wait if the file has
	// not been set to blocking mode, as otherwise any current I/O2
	// may be blocking, and that would block the Close.
	// No need for an atomic read of isBlocking, increfAndClose means
	// we have exclusive access to fd.
	// 等待系统fd关闭，如果本FD是唯一引用该系统fd的，则该系统fd理论上是已经关闭了。
	// 只有当FD是非阻塞模式时，才会等待。因为如果当前还被IO阻塞等待，那么本Close就会因此而被阻塞住
	// 不需要原子读fd.isBlocking字段的值，因为上面的fd.fdmu.increfAndClose()已经做了互斥保护
	if fd.isBlocking == 0 { //非阻塞模式
		//等待系统fd关闭的信号，只在非阻塞模式时才会等待。因为在阻塞模式下，只要有一个IO还在阻塞中，就无法调用本方法CLose()进行关闭
		runtime_Semacquire(&fd.csema) //等待系统fd关闭的信号，以确保系统fd关闭完成后，本FD实例才继续执行剩下的关闭逻辑
	}

	return err
}

// SetBlocking 把FD设为阻塞模式
func (fd *FD) SetBlocking() error {
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	// Atomic store so that concurrent calls to SetBlocking
	// do not cause a race condition. isBlocking only ever goes
	// from 0 to 1 so there is no real race here.
	// 原子保存，因此并发调用本函数也不会导致竞态
	atomic.StoreUint32(&fd.isBlocking, 1)
	return syscall.SetNonblock(fd.Sysfd, false) //通过系统调用把fd设为非阻塞模式
}

// Darwin and FreeBSD can't read or write 2GB+ files at a time,
// even on 64-bit systems.
// The same is true of socket implementations on many systems.
// See golang.org/issue/7812 and golang.org/issue/16266.
// Use 1GB instead of, say, 2GB-1, to keep subsequent reads aligned.
const maxRW = 1 << 30

// Read implements io.Reader.
// 尝试从fd里读取数据，当无数据且是非阻塞时，则会polling，并挂起当前g
func (fd *FD) Read(p []byte) (int, error) {
	if err := fd.readLock(); err != nil {
		return 0, err
	}
	defer fd.readUnlock()
	if len(p) == 0 {
		// If the caller wanted a zero byte read, return immediately
		// without trying (but after acquiring the readLock).
		// Otherwise syscall.Read returns 0, nil which looks like
		// io.EOF.
		// TODO(bradfitz): make it wait for readability? (Issue 15735)
		return 0, nil
	}
	if err := fd.pd.prepareRead(fd.isFile); err != nil { //如果对应底层的fd有异常，则退出
		return 0, err
	}
	if fd.IsStream && len(p) > maxRW { //如果是TCP，则一次调用最大返回数据量是maxRW
		p = p[:maxRW]
	}
	for {
		n, err := ignoringEINTRIO(syscall.Read, fd.Sysfd, p) //非阻塞模式，调用底层的系统read后，立即返回。这里尝试调用一次，看看该fd是否有数据可读了
		if err != nil {                                      //未有数据可读，则进入polling
			n = 0
			if err == syscall.EAGAIN && fd.pd.pollable() { //如果是pollable的，则进入polling等待
				if err = fd.pd.waitRead(fd.isFile); err == nil { //底层调用runtime_pollWait，go会在这挂起（gopark），直到有网络事件
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, err
	}
}

// Pread wraps the pread system call.
func (fd *FD) Pread(p []byte, off int64) (int, error) {
	// Call incref, not readLock, because since pread specifies the
	// offset it is independent from other reads.
	// Similarly, using the poller doesn't make sense for pread.
	if err := fd.incref(); err != nil {
		return 0, err
	}
	if fd.IsStream && len(p) > maxRW {
		p = p[:maxRW]
	}
	var (
		n   int
		err error
	)
	for {
		n, err = syscall.Pread(fd.Sysfd, p, off)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		n = 0
	}
	fd.decref()
	err = fd.eofError(n, err)
	return n, err
}

// ReadFrom wraps the recvfrom network call.
// ReadFrom 对 底层系统recvfrom函数 的封装
func (fd *FD) ReadFrom(p []byte) (int, syscall.Sockaddr, error) {
	if err := fd.readLock(); err != nil { //阻塞获取读锁
		return 0, nil, err
	}
	defer fd.readUnlock()
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, nil, err
	}
	for {
		n, sa, err := syscall.Recvfrom(fd.Sysfd, p, 0) //先初尝读取数据
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			n = 0
			if err == syscall.EAGAIN && fd.pd.pollable() { //暂无数据可读，则进行poll等待
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, sa, err
	}
}

// ReadFromInet4 wraps the recvfrom network call for IPv4.
func (fd *FD) ReadFromInet4(p []byte, from *syscall.SockaddrInet4) (int, error) {
	if err := fd.readLock(); err != nil {
		return 0, err
	}
	defer fd.readUnlock()
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, err
	}
	for {
		n, err := unix.RecvfromInet4(fd.Sysfd, p, 0, from)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			n = 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, err
	}
}

// ReadFromInet6 wraps the recvfrom network call for IPv6.
func (fd *FD) ReadFromInet6(p []byte, from *syscall.SockaddrInet6) (int, error) {
	if err := fd.readLock(); err != nil {
		return 0, err
	}
	defer fd.readUnlock()
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, err
	}
	for {
		n, err := unix.RecvfromInet6(fd.Sysfd, p, 0, from)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			n = 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, err
	}
}

// ReadMsg wraps the recvmsg network call.
func (fd *FD) ReadMsg(p []byte, oob []byte, flags int) (int, int, int, syscall.Sockaddr, error) {
	if err := fd.readLock(); err != nil {
		return 0, 0, 0, nil, err
	}
	defer fd.readUnlock()
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, 0, 0, nil, err
	}
	for {
		n, oobn, sysflags, sa, err := syscall.Recvmsg(fd.Sysfd, p, oob, flags)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			// TODO(dfc) should n and oobn be set to 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, oobn, sysflags, sa, err
	}
}

// ReadMsgInet4 is ReadMsg, but specialized for syscall.SockaddrInet4.
func (fd *FD) ReadMsgInet4(p []byte, oob []byte, flags int, sa4 *syscall.SockaddrInet4) (int, int, int, error) {
	if err := fd.readLock(); err != nil {
		return 0, 0, 0, err
	}
	defer fd.readUnlock()
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, 0, 0, err
	}
	for {
		n, oobn, sysflags, err := unix.RecvmsgInet4(fd.Sysfd, p, oob, flags, sa4)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			// TODO(dfc) should n and oobn be set to 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, oobn, sysflags, err
	}
}

// ReadMsgInet6 is ReadMsg, but specialized for syscall.SockaddrInet6.
func (fd *FD) ReadMsgInet6(p []byte, oob []byte, flags int, sa6 *syscall.SockaddrInet6) (int, int, int, error) {
	if err := fd.readLock(); err != nil {
		return 0, 0, 0, err
	}
	defer fd.readUnlock()
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, 0, 0, err
	}
	for {
		n, oobn, sysflags, err := unix.RecvmsgInet6(fd.Sysfd, p, oob, flags, sa6)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			// TODO(dfc) should n and oobn be set to 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, oobn, sysflags, err
	}
}

// Write implements io.Writer.
func (fd *FD) Write(p []byte) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, err
	}
	var nn int
	for {
		max := len(p)
		if fd.IsStream && max-nn > maxRW {
			max = nn + maxRW
		}
		n, err := ignoringEINTRIO(syscall.Write, fd.Sysfd, p[nn:max])
		if n > 0 {
			nn += n
		}
		if nn == len(p) {
			return nn, err
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue
			}
		}
		if err != nil {
			return nn, err
		}
		if n == 0 {
			return nn, io.ErrUnexpectedEOF
		}
	}
}

// Pwrite wraps the pwrite system call.
func (fd *FD) Pwrite(p []byte, off int64) (int, error) {
	// Call incref, not writeLock, because since pwrite specifies the
	// offset it is independent from other writes.
	// Similarly, using the poller doesn't make sense for pwrite.
	if err := fd.incref(); err != nil {
		return 0, err
	}
	defer fd.decref()
	var nn int
	for {
		max := len(p)
		if fd.IsStream && max-nn > maxRW {
			max = nn + maxRW
		}
		n, err := syscall.Pwrite(fd.Sysfd, p[nn:max], off+int64(nn))
		if err == syscall.EINTR {
			continue
		}
		if n > 0 {
			nn += n
		}
		if nn == len(p) {
			return nn, err
		}
		if err != nil {
			return nn, err
		}
		if n == 0 {
			return nn, io.ErrUnexpectedEOF
		}
	}
}

// WriteToInet4 wraps the sendto network call for IPv4 addresses.
func (fd *FD) WriteToInet4(p []byte, sa *syscall.SockaddrInet4) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, err
	}
	for {
		err := unix.SendtoInet4(fd.Sysfd, p, 0, sa)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue
			}
		}
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
}

// WriteToInet6 wraps the sendto network call for IPv6 addresses.
func (fd *FD) WriteToInet6(p []byte, sa *syscall.SockaddrInet6) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, err
	}
	for {
		err := unix.SendtoInet6(fd.Sysfd, p, 0, sa)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue
			}
		}
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
}

// WriteTo wraps the sendto network call.
func (fd *FD) WriteTo(p []byte, sa syscall.Sockaddr) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, err
	}
	for {
		err := syscall.Sendto(fd.Sysfd, p, 0, sa)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue
			}
		}
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
}

// WriteMsg wraps the sendmsg network call.
func (fd *FD) WriteMsg(p []byte, oob []byte, sa syscall.Sockaddr) (int, int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, 0, err
	}
	for {
		n, err := syscall.SendmsgN(fd.Sysfd, p, oob, sa, 0)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue
			}
		}
		if err != nil {
			return n, 0, err
		}
		return n, len(oob), err
	}
}

// WriteMsgInet4 is WriteMsg specialized for syscall.SockaddrInet4.
func (fd *FD) WriteMsgInet4(p []byte, oob []byte, sa *syscall.SockaddrInet4) (int, int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, 0, err
	}
	for {
		n, err := unix.SendmsgNInet4(fd.Sysfd, p, oob, sa, 0)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue
			}
		}
		if err != nil {
			return n, 0, err
		}
		return n, len(oob), err
	}
}

// WriteMsgInet6 is WriteMsg specialized for syscall.SockaddrInet6.
func (fd *FD) WriteMsgInet6(p []byte, oob []byte, sa *syscall.SockaddrInet6) (int, int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, 0, err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return 0, 0, err
	}
	for {
		n, err := unix.SendmsgNInet6(fd.Sysfd, p, oob, sa, 0)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN && fd.pd.pollable() {
			if err = fd.pd.waitWrite(fd.isFile); err == nil {
				continue
			}
		}
		if err != nil {
			return n, 0, err
		}
		return n, len(oob), err
	}
}

// Accept wraps the accept network call.
// Accept 封装底层网络调用accept
func (fd *FD) Accept() (int, syscall.Sockaddr, string, error) {
	if err := fd.readLock(); err != nil {
		return -1, nil, "", err
	}
	defer fd.readUnlock()

	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return -1, nil, "", err
	}
	for {
		s, rsa, errcall, err := accept(fd.Sysfd)
		if err == nil { //当有新连接时，走这里
			return s, rsa, "", err
		}
		switch err {
		case syscall.EINTR:
			//系统中断
			continue
		case syscall.EAGAIN:
			//非阻塞模式，当无连接时走这里
			if fd.pd.pollable() { //是可poll的
				//当没有新的连接请求时，则当前协程进入gopark
				if err = fd.pd.waitRead(fd.isFile); err == nil { //g阻塞在这

					//网络有io，此时本协程被唤醒，从这里开始执行，并继续执行后续逻辑，continue回到上面accept，获取到一个新连接
					continue
				}
			}
		case syscall.ECONNABORTED:
			// 在backlog队列里等待被accept的socket还没来得及被accept就关闭了，
			// 此时就会出现本错误走到本路径
			// This means that a socket on the listen
			// queue was closed before we Accept()ed it;
			// it's a silly error, so try again.
			continue
		}
		return -1, nil, errcall, err
	}
}

// Seek wraps syscall.Seek.
func (fd *FD) Seek(offset int64, whence int) (int64, error) {
	if err := fd.incref(); err != nil {
		return 0, err
	}
	defer fd.decref()
	return syscall.Seek(fd.Sysfd, offset, whence)
}

// ReadDirent wraps syscall.ReadDirent.
// We treat this like an ordinary system call rather than a call
// that tries to fill the buffer.
func (fd *FD) ReadDirent(buf []byte) (int, error) {
	if err := fd.incref(); err != nil {
		return 0, err
	}
	defer fd.decref()
	for {
		n, err := ignoringEINTRIO(syscall.ReadDirent, fd.Sysfd, buf)
		if err != nil {
			n = 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}
		}
		// Do not call eofError; caller does not expect to see io.EOF.
		return n, err
	}
}

// Fchmod wraps syscall.Fchmod.
func (fd *FD) Fchmod(mode uint32) error {
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	return ignoringEINTR(func() error {
		return syscall.Fchmod(fd.Sysfd, mode)
	})
}

// Fchdir wraps syscall.Fchdir.
func (fd *FD) Fchdir() error {
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	return syscall.Fchdir(fd.Sysfd)
}

// Fstat wraps syscall.Fstat
func (fd *FD) Fstat(s *syscall.Stat_t) error {
	if err := fd.incref(); err != nil {
		return err
	}
	defer fd.decref()
	return ignoringEINTR(func() error {
		return syscall.Fstat(fd.Sysfd, s)
	})
}

// tryDupCloexec indicates whether F_DUPFD_CLOEXEC should be used.
// If the kernel doesn't support it, this is set to 0.
var tryDupCloexec = int32(1)

// DupCloseOnExec dups fd and marks it close-on-exec.
func DupCloseOnExec(fd int) (int, string, error) {
	if syscall.F_DUPFD_CLOEXEC != 0 && atomic.LoadInt32(&tryDupCloexec) == 1 {
		r0, e1 := fcntl(fd, syscall.F_DUPFD_CLOEXEC, 0)
		if e1 == nil {
			return r0, "", nil
		}
		switch e1.(syscall.Errno) {
		case syscall.EINVAL, syscall.ENOSYS:
			// Old kernel, or js/wasm (which returns
			// ENOSYS). Fall back to the portable way from
			// now on.
			atomic.StoreInt32(&tryDupCloexec, 0)
		default:
			return -1, "fcntl", e1
		}
	}
	return dupCloseOnExecOld(fd)
}

// dupCloseOnExecOld is the traditional way to dup an fd and
// set its O_CLOEXEC bit, using two system calls.
func dupCloseOnExecOld(fd int) (int, string, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	newfd, err := syscall.Dup(fd)
	if err != nil {
		return -1, "dup", err
	}
	syscall.CloseOnExec(newfd)
	return newfd, "", nil
}

// Dup duplicates the file descriptor.
func (fd *FD) Dup() (int, string, error) {
	if err := fd.incref(); err != nil {
		return -1, "", err
	}
	defer fd.decref()
	return DupCloseOnExec(fd.Sysfd)
}

// On Unix variants only, expose the IO event for the net code.

// WaitWrite waits until data can be read from fd.
func (fd *FD) WaitWrite() error {
	return fd.pd.waitWrite(fd.isFile)
}

// WriteOnce is for testing only. It makes a single write call.
func (fd *FD) WriteOnce(p []byte) (int, error) {
	if err := fd.writeLock(); err != nil {
		return 0, err
	}
	defer fd.writeUnlock()
	return ignoringEINTRIO(syscall.Write, fd.Sysfd, p)
}

// RawRead invokes the user-defined function f for a read operation.
func (fd *FD) RawRead(f func(uintptr) bool) error {
	if err := fd.readLock(); err != nil {
		return err
	}
	defer fd.readUnlock()
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return err
	}
	for {
		if f(uintptr(fd.Sysfd)) {
			return nil
		}
		if err := fd.pd.waitRead(fd.isFile); err != nil {
			return err
		}
	}
}

// RawWrite invokes the user-defined function f for a write operation.
func (fd *FD) RawWrite(f func(uintptr) bool) error {
	if err := fd.writeLock(); err != nil {
		return err
	}
	defer fd.writeUnlock()
	if err := fd.pd.prepareWrite(fd.isFile); err != nil {
		return err
	}
	for {
		if f(uintptr(fd.Sysfd)) {
			return nil
		}
		if err := fd.pd.waitWrite(fd.isFile); err != nil {
			return err
		}
	}
}

// ignoringEINTRIO is like ignoringEINTR, but just for IO calls.
func ignoringEINTRIO(fn func(fd int, p []byte) (int, error), fd int, p []byte) (int, error) {
	for {
		n, err := fn(fd, p)
		if err != syscall.EINTR {
			return n, err
		}
	}
}
