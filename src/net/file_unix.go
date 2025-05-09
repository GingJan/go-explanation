// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package net

import (
	"internal/poll"
	"os"
	"syscall"
)

//把f里的系统fd复制一份副本，并返回该副本
func dupSocket(f *os.File) (int, error) {
	dupS, call, err := poll.DupCloseOnExec(int(f.Fd())) //底层系统fd的副本 dupS
	if err != nil {
		if call != "" {
			err = os.NewSyscallError(call, err)
		}
		return -1, err
	}

	//把系统fd（副本）设为非阻塞
	if err := syscall.SetNonblock(dupS, true); err != nil {
		poll.CloseFunc(dupS)
		return -1, os.NewSyscallError("setnonblock", err)
	}

	return dupS, nil
}

//把传入的fd，封装成一个网络fd，并创建该结构体的实例
func newFileFD(f *os.File) (*netFD, error) {
	dupS, err := dupSocket(f) //f的副本 dupS
	if err != nil {
		return nil, err
	}
	family := syscall.AF_UNSPEC
	sotype, err := syscall.GetsockoptInt(dupS, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil {
		poll.CloseFunc(dupS)
		return nil, os.NewSyscallError("getsockopt", err)
	}

	lsa, _ := syscall.Getsockname(dupS)
	rsa, _ := syscall.Getpeername(dupS)
	switch lsa.(type) {
	case *syscall.SockaddrInet4:
		family = syscall.AF_INET
	case *syscall.SockaddrInet6:
		family = syscall.AF_INET6
	case *syscall.SockaddrUnix:
		family = syscall.AF_UNIX
	default:
		poll.CloseFunc(dupS)
		return nil, syscall.EPROTONOSUPPORT
	}

	//新建一个封装了dupS的结构体实例
	netFd, err := newFD(dupS, family, sotype, "")
	if err != nil {
		poll.CloseFunc(dupS)
		return nil, err
	}
	laddr := netFd.addrFunc()(lsa)
	raddr := netFd.addrFunc()(rsa)
	netFd.net = laddr.Network()
	if err := netFd.init(); err != nil {
		netFd.Close()
		return nil, err
	}

	netFd.setAddr(laddr, raddr)
	return netFd, nil
}

func fileConn(f *os.File) (Conn, error) {
	fd, err := newFileFD(f)
	if err != nil {
		return nil, err
	}
	switch fd.laddr.(type) {
	case *TCPAddr: //如果指定的地址是网络地址
		return newTCPConn(fd), nil
	case *UDPAddr:
		return newUDPConn(fd), nil
	case *IPAddr:
		return newIPConn(fd), nil
	case *UnixAddr:
		return newUnixConn(fd), nil
	}
	fd.Close()
	return nil, syscall.EINVAL
}

func fileListener(f *os.File) (Listener, error) {
	fd, err := newFileFD(f)
	if err != nil {
		return nil, err
	}
	switch laddr := fd.laddr.(type) {
	case *TCPAddr:
		return &TCPListener{fd: fd}, nil
	case *UnixAddr:
		return &UnixListener{fd: fd, path: laddr.Name, unlink: false}, nil
	}
	fd.Close()
	return nil, syscall.EINVAL
}

func filePacketConn(f *os.File) (PacketConn, error) {
	fd, err := newFileFD(f)
	if err != nil {
		return nil, err
	}
	switch fd.laddr.(type) {
	case *UDPAddr:
		return newUDPConn(fd), nil
	case *IPAddr:
		return newIPConn(fd), nil
	case *UnixAddr:
		return newUnixConn(fd), nil
	}
	fd.Close()
	return nil, syscall.EINVAL
}
