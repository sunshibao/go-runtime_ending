// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/sys"
	"unsafe"
)

// PROT_NONE 页不可访问
// PROT_READ 页内容可以被读取
// PROT_WRITE 页可以被写入

// MAP_ANONYMOUS 匿名映射，映射区不与任何文件关联。
// MAP_ANON MAP_ANONYMOUS的别称，不再被使用。
// MAP_PRIVATE 建立一个写入时拷贝的私有映射。内存区域的写入不会影响到原文件。
// MAP_FIXED 使用指定的映射起始地址，如果由start和len参数指定的内存区重叠于现存的映射空间，重叠部分将会被丢弃。
// 如果指定的起始地址不可用，操作将会失败。并且起始地址必须落在页的边界上。

const (
	_EACCES = 13
	_EINVAL = 22
)

// NOTE: vec must be just 1 byte long here.
// Mincore returns ENOMEM if any of the pages are unmapped,
// but we want to know that all of the pages are unmapped.
// To make these the same, we can only ask about one page
// at a time. See golang.org/issue/7476.
var addrspace_vec [1]byte

func addrspace_free(v unsafe.Pointer, n uintptr) bool {
	for off := uintptr(0); off < n; off += physPageSize {
		// Use a length of 1 byte, which the kernel will round
		// up to one physical page regardless of the true
		// physical page size.
		errval := mincore(unsafe.Pointer(uintptr(v)+off), 1, &addrspace_vec[0])
		if errval == -_EINVAL {
			// Address is not a multiple of the physical
			// page size. Shouldn't happen, but just ignore it.
			continue
		}
		// ENOMEM means unmapped, which is what we want.
		// Anything else we assume means the pages are mapped.
		if errval != -_ENOMEM {
			return false
		}
	}
	return true
}

func mmap_fixed(v unsafe.Pointer, n uintptr, prot, flags, fd int32, offset uint32) (unsafe.Pointer, int) {
	p, err := mmap(v, n, prot, flags, fd, offset)
	// On some systems, mmap ignores v without
	// MAP_FIXED, so retry if the address space is free.
	if p != v && addrspace_free(v, n) {
		if err == 0 {
			munmap(p, n)
		}
		p, err = mmap(v, n, prot, flags|_MAP_FIXED, fd, offset)
	}
	return p, err
}

// Don't split the stack as this method may be invoked without a valid G, which
// prevents us from allocating more stack.
//go:nosplit
func sysAlloc(n uintptr, sysStat *uint64) unsafe.Pointer {
	p, err := mmap(nil, n, _PROT_READ|_PROT_WRITE, _MAP_ANON|_MAP_PRIVATE, -1, 0)
	if err != 0 {
		if err == _EACCES {
			print("runtime: mmap: access denied\n")
			exit(2)
		}
		if err == _EAGAIN {
			print("runtime: mmap: too much locked memory (check 'ulimit -l').\n")
			exit(2)
		}
		return nil
	}
	mSysStatInc(sysStat, n)
	return p
}

func sysUnused(v unsafe.Pointer, n uintptr) {
	// By default, Linux's "transparent huge page" support will
	// merge pages into a huge page if there's even a single
	// present regular page, undoing the effects of the DONTNEED
	// below. On amd64, that means khugepaged can turn a single
	// 4KB page to 2MB, bloating the process's RSS by as much as
	// 512X. (See issue #8832 and Linux kernel bug
	// https://bugzilla.kernel.org/show_bug.cgi?id=93111)
	//
	// To work around this, we explicitly disable transparent huge
	// pages when we release pages of the heap. However, we have
	// to do this carefully because changing this flag tends to
	// split the VMA (memory mapping) containing v in to three
	// VMAs in order to track the different values of the
	// MADV_NOHUGEPAGE flag in the different regions. There's a
	// default limit of 65530 VMAs per address space (sysctl
	// vm.max_map_count), so we must be careful not to create too
	// many VMAs (see issue #12233).
	//
	// Since huge pages are huge, there's little use in adjusting
	// the MADV_NOHUGEPAGE flag on a fine granularity, so we avoid
	// exploding the number of VMAs by only adjusting the
	// MADV_NOHUGEPAGE flag on a large granularity. This still
	// gets most of the benefit of huge pages while keeping the
	// number of VMAs under control. With hugePageSize = 2MB, even
	// a pessimal heap can reach 128GB before running out of VMAs.
	if sys.HugePageSize != 0 {
		var s uintptr = sys.HugePageSize // division by constant 0 is a compile-time error :(

		// If it's a large allocation, we want to leave huge
		// pages enabled. Hence, we only adjust the huge page
		// flag on the huge pages containing v and v+n-1, and
		// only if those aren't aligned.
		var head, tail uintptr
		if uintptr(v)%s != 0 {
			// Compute huge page containing v.
			head = uintptr(v) &^ (s - 1)
		}
		if (uintptr(v)+n)%s != 0 {
			// Compute huge page containing v+n-1.
			tail = (uintptr(v) + n - 1) &^ (s - 1)
		}

		// Note that madvise will return EINVAL if the flag is
		// already set, which is quite likely. We ignore
		// errors.
		if head != 0 && head+sys.HugePageSize == tail {
			// head and tail are different but adjacent,
			// so do this in one call.
			madvise(unsafe.Pointer(head), 2*sys.HugePageSize, _MADV_NOHUGEPAGE)
		} else {
			// Advise the huge pages containing v and v+n-1.
			if head != 0 {
				madvise(unsafe.Pointer(head), sys.HugePageSize, _MADV_NOHUGEPAGE)
			}
			if tail != 0 && tail != head {
				madvise(unsafe.Pointer(tail), sys.HugePageSize, _MADV_NOHUGEPAGE)
			}
		}
	}

	if uintptr(v)&(physPageSize-1) != 0 || n&(physPageSize-1) != 0 {
		// madvise will round this to any physical page
		// *covered* by this range, so an unaligned madvise
		// will release more memory than intended.
		throw("unaligned sysUnused")
	}

	madvise(v, n, _MADV_DONTNEED)
}

func sysUsed(v unsafe.Pointer, n uintptr) {
	if sys.HugePageSize != 0 {
		// Partially undo the NOHUGEPAGE marks from sysUnused
		// for whole huge pages between v and v+n. This may
		// leave huge pages off at the end points v and v+n
		// even though allocations may cover these entire huge
		// pages. We could detect this and undo NOHUGEPAGE on
		// the end points as well, but it's probably not worth
		// the cost because when neighboring allocations are
		// freed sysUnused will just set NOHUGEPAGE again.
		var s uintptr = sys.HugePageSize

		// Round v up to a huge page boundary.
		beg := (uintptr(v) + (s - 1)) &^ (s - 1)
		// Round v+n down to a huge page boundary.
		end := (uintptr(v) + n) &^ (s - 1)

		if beg < end {
			madvise(unsafe.Pointer(beg), end-beg, _MADV_HUGEPAGE)
		}
	}
}

// Don't split the stack as this function may be invoked without a valid G,
// which prevents us from allocating more stack.
//go:nosplit
func sysFree(v unsafe.Pointer, n uintptr, sysStat *uint64) {
	mSysStatDec(sysStat, n)
	munmap(v, n)
}

func sysFault(v unsafe.Pointer, n uintptr) {
	mmap(v, n, _PROT_NONE, _MAP_ANON|_MAP_PRIVATE|_MAP_FIXED, -1, 0)
}

// sysReserve 预留一段内存(未分配),如果参数非空，说么调用者希望从这里开始预留，
// 但是sysReserve 仍然可以选择另一个位置如果希望的位置不可用，
// 有些os上某些情况 sysReserve 仅仅检查位置是否可用而并不真正的预留它，
// 当sysReserve返回非空时，如果这段内存预留了，它会设置 *reserved为true；
// 否则设置为false，这意味着仅仅做了检查，没有预留
// 注意：SysReserve 返回OS对齐的内存，但堆分配器可能使用更大的对齐，
// 因此编码器必须小心重新调整 sysAlloc 获取的内存。
func sysReserve(v unsafe.Pointer, n uintptr, reserved *bool) unsafe.Pointer {
	// On 64-bit, people with ulimit -v set complain if we reserve too
	// much address space. Instead, assume that the reservation is okay
	// if we can reserve at least 64K and check the assumption in SysMap.
	// Only user-mode Linux (UML) rejects these requests.
	// 在64位上，如果我们保留了太多地址空间，那么ulimit -v设置的人会抱怨。
	// 相反，如果我们可以保留至少64K并在SysMap中检查假设，则假设保留是可以的。
	// 只有用户模式Linux（UML）拒绝这些请求。
	if sys.PtrSize == 8 && uint64(n) > 1<<32 {
		p, err := mmap_fixed(v, 64<<10, _PROT_NONE, _MAP_ANON|_MAP_PRIVATE, -1, 0)
		if p != v || err != 0 {
			if err == 0 {
				munmap(p, 64<<10)
			}
			return nil
		}
		munmap(p, 64<<10)
		*reserved = false
		return v
	}

	p, err := mmap(v, n, _PROT_NONE, _MAP_ANON|_MAP_PRIVATE, -1, 0)
	if err != 0 {
		return nil
	}
	*reserved = true
	return p
}

// 分配虚拟内存，没有分配物理内存。在第一次访问已分配的虚拟地址空间的时候，发生缺页中断，
// 操作系统负责分配物理内存，然后建立虚拟内存和物理内存之间的映射关系。
func sysMap(v unsafe.Pointer, n uintptr, reserved bool, sysStat *uint64) {
	mSysStatInc(sysStat, n)

	// On 64-bit, we don't actually have v reserved, so tread carefully.
	if !reserved {
		p, err := mmap_fixed(v, n, _PROT_READ|_PROT_WRITE, _MAP_ANON|_MAP_PRIVATE, -1, 0)
		if err == _ENOMEM {
			throw("runtime: out of memory")
		}
		if p != v || err != 0 {
			print("runtime: address space conflict: map(", v, ") = ", p, " (err ", err, ")\n")
			throw("runtime: address space conflict")
		}
		return
	}

	p, err := mmap(v, n, _PROT_READ|_PROT_WRITE, _MAP_ANON|_MAP_FIXED|_MAP_PRIVATE, -1, 0)
	if err == _ENOMEM {
		throw("runtime: out of memory")
	}
	if p != v || err != 0 {
		throw("runtime: cannot map pages in arena address space")
	}
}
