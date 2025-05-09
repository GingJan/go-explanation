// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package atomic

import (
	"unsafe"
)

// A Value provides an atomic load and store of a consistently typed value.
// The zero value for a Value returns nil from Load.
// Once Store has been called, a Value must not be copied.
//
// A Value must not be copied after first use.
type Value struct {
	v any
}

// ifaceWords is interface{} internal representation.
type ifaceWords struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

// Load returns the value set by the most recent Store.
// It returns nil if there has been no call to Store for this Value.
func (v *Value) Load() (val any) {
	vp := (*ifaceWords)(unsafe.Pointer(v)) //目的：把v转为ifaceWords，以便后续的操作，（v的.v是any）

	typ := LoadPointer(&vp.typ)

	//类型为nil或处于中间态，说明还没首次赋值或者正在进行首次赋值
	if typ == nil || typ == unsafe.Pointer(&firstStoreInProgress) {
		// 首次赋值未完成
		return nil
	}

	data := LoadPointer(&vp.data)

	//把v的类型和值赋给val返回
	vlp := (*ifaceWords)(unsafe.Pointer(&val))
	vlp.typ = typ
	vlp.data = data
	return
}

var firstStoreInProgress byte //代表正在进行首次存储中的中间态

// 原子赋值给v，首次赋值会涉及到类型设置，因此有个中间态需等待
func (v *Value) Store(val any) {
	if val == nil {
		panic("sync/atomic: store of nil value into Value")
	}
	vp := (*ifaceWords)(unsafe.Pointer(v))
	vlp := (*ifaceWords)(unsafe.Pointer(&val))
	for {
		typ := LoadPointer(&vp.typ)
		if typ == nil {
			// 开始第一次赋值操作
			// 禁止调度器对当前 goroutine 的抢占，使得它在执行当前逻辑的时候不被打断，以便可以尽快地完成工作，因为别人一直在等待它。
			// 另一方面，在禁止抢占期间，GC 线程也无法被启用，这样可以防止 GC 线程看到一个莫名其妙的指向firstStoreInProgress的类型（这是赋值过程中的中间状态）
			runtime_procPin()
			if !CompareAndSwapPointer(&vp.typ, nil, unsafe.Pointer(&firstStoreInProgress)) { //先设为中间态「firstStoreInProgress」
				runtime_procUnpin()
				continue
			}
			// 因为首次赋值涉及到两个字段的操作，一个是类型typ，一个是数据data，需要两次赋值操作，所以需要中间态过度
			// 完成Value的首次赋值（首次赋值涉及到类型设置）
			StorePointer(&vp.data, vlp.data)
			StorePointer(&vp.typ, vlp.typ)
			runtime_procUnpin() //恢复抢占
			return
		}
		if typ == unsafe.Pointer(&firstStoreInProgress) { //如果首次存储还在进行中，则返回继续循环等待
			// 因为在Value首次赋值时关闭了抢占，所以我们可以通过自旋方式等待首次赋值完成后，继续操作
			continue
		}
		// First store completed. Check type and overwrite data.

		// 新值类型和旧值类型不同
		if typ != vlp.typ {
			panic("sync/atomic: store of inconsistently typed value into Value")
		}

		// 更新值
		StorePointer(&vp.data, vlp.data)
		return
	}
}

// Swap stores new into Value and returns the previous value. It returns nil if
// the Value is empty.
//
// All calls to Swap for a given Value must use values of the same concrete
// type. Swap of an inconsistent type panics, as does Swap(nil).
func (v *Value) Swap(new any) (old any) {
	if new == nil {
		panic("sync/atomic: swap of nil value into Value")
	}
	vp := (*ifaceWords)(unsafe.Pointer(v))
	np := (*ifaceWords)(unsafe.Pointer(&new))
	for {
		typ := LoadPointer(&vp.typ)
		if typ == nil { //v还没首次赋值
			// 则先进行首次赋值
			// 关闭本协程的可抢占标识，其他协程可自旋等待本协程进行的首次赋值完成
			// 此时GC就不会遇到异常情况

			//关闭抢占
			runtime_procPin()

			//更新状态为中间态
			if !CompareAndSwapPointer(&vp.typ, nil, unsafe.Pointer(&firstStoreInProgress)) {
				runtime_procUnpin()
				continue
			}

			// 进行首次赋值
			StorePointer(&vp.data, np.data)
			StorePointer(&vp.typ, np.typ)

			// 赋值完成，恢复抢占
			runtime_procUnpin()
			return nil
		}
		if typ == unsafe.Pointer(&firstStoreInProgress) {
			// First store in progress. Wait.
			// Since we disable preemption around the first store,
			// we can wait with active spinning.
			continue
		}
		// First store completed. Check type and overwrite data.
		if typ != np.typ {
			panic("sync/atomic: swap of inconsistently typed value into Value")
		}
		op := (*ifaceWords)(unsafe.Pointer(&old))
		op.typ, op.data = np.typ, SwapPointer(&vp.data, np.data)
		return old
	}
}

// CompareAndSwap executes the compare-and-swap operation for the Value.
//
// All calls to CompareAndSwap for a given Value must use values of the same
// concrete type. CompareAndSwap of an inconsistent type panics, as does
// CompareAndSwap(old, nil).
func (v *Value) CompareAndSwap(old, new any) (swapped bool) {
	if new == nil {
		panic("sync/atomic: compare and swap of nil value into Value")
	}
	vp := (*ifaceWords)(unsafe.Pointer(v))
	np := (*ifaceWords)(unsafe.Pointer(&new))
	op := (*ifaceWords)(unsafe.Pointer(&old))
	if op.typ != nil && np.typ != op.typ {
		panic("sync/atomic: compare and swap of inconsistently typed values")
	}
	for {
		typ := LoadPointer(&vp.typ)
		if typ == nil {
			if old != nil {
				return false
			}
			// Attempt to start first store.
			// Disable preemption so that other goroutines can use
			// active spin wait to wait for completion; and so that
			// GC does not see the fake type accidentally.
			runtime_procPin()
			if !CompareAndSwapPointer(&vp.typ, nil, unsafe.Pointer(&firstStoreInProgress)) {
				runtime_procUnpin()
				continue
			}
			// Complete first store.
			StorePointer(&vp.data, np.data)
			StorePointer(&vp.typ, np.typ)
			runtime_procUnpin()
			return true
		}
		if typ == unsafe.Pointer(&firstStoreInProgress) {
			// First store in progress. Wait.
			// Since we disable preemption around the first store,
			// we can wait with active spinning.
			continue
		}
		// First store completed. Check type and overwrite data.
		if typ != np.typ {
			panic("sync/atomic: compare and swap of inconsistently typed value into Value")
		}
		// Compare old and current via runtime equality check.
		// This allows value types to be compared, something
		// not offered by the package functions.
		// CompareAndSwapPointer below only ensures vp.data
		// has not changed since LoadPointer.
		data := LoadPointer(&vp.data)
		var i any
		(*ifaceWords)(unsafe.Pointer(&i)).typ = typ
		(*ifaceWords)(unsafe.Pointer(&i)).data = data
		if i != old {
			return false
		}
		return CompareAndSwapPointer(&vp.data, data, np.data)
	}
}

// Disable/enable preemption, implemented in runtime.
func runtime_procPin() //禁用协程抢占，在编译时，会替换成 runtime.procPin
func runtime_procUnpin()
