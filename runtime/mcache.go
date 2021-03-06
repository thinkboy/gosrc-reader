// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// Per-thread (in Go, per-P) cache for small objects.
// No locking needed because it is per-thread (per-P).
//
// mcaches are allocated from non-GC'd memory, so any heap pointers
// must be specially handled.
//
//go:notinheap
type mcache struct {
	// The following members are accessed on every malloc,
	// so they are grouped here for better caching.
	next_sample int32   // trigger heap sample after allocating this many bytes
	local_scan  uintptr // bytes of scannable heap allocated

	// Allocator cache for tiny objects w/o pointers.
	// See "Tiny allocator" comment in malloc.go.

	// tiny points to the beginning of the current tiny block, or
	// nil if there is no current tiny block.
	//
	// tiny is a heap pointer. Since mcache is in non-GC'd memory,
	// we handle it by clearing it in releaseAll during mark
	// termination.
	tiny             uintptr // 16字节tiny的内存指针
	tinyoffset       uintptr // 已经使用到的内存地址便宜
	local_tinyallocs uintptr // 通过tiny分配内存的次数 number of tiny allocs not counted in other stats

	// The rest is not accessed on every malloc.

	alloc [numSpanClasses]*mspan // cache住的不同大小内存块的span // spans to allocate from, indexed by spanClass

	stackcache [_NumStackOrders]stackfreelist

	// Local allocator stats, flushed during GC.
	local_nlookup    uintptr                  // number of pointer lookups
	local_largefree  uintptr                  // bytes freed for large objects (>maxsmallsize)
	local_nlargefree uintptr                  // number of frees for large objects (>maxsmallsize)
	local_nsmallfree [_NumSizeClasses]uintptr // 小对象(<=maxsmallsize 32KB)里面返还给mheap或者mcentral的未被使用的object的历史总和，但是当pprof获取堆信息的时候会被清0 // number of frees for small objects (<=maxsmallsize)
}

// A gclink is a node in a linked list of blocks, like mlink,
// but it is opaque to the garbage collector.
// The GC does not trace the pointers during collection,
// and the compiler does not emit write barriers for assignments
// of gclinkptr values. Code should store references to gclinks
// as gclinkptr, not as *gclink.
type gclink struct {
	next gclinkptr
}

// A gclinkptr is a pointer to a gclink, but it is opaque
// to the garbage collector.
type gclinkptr uintptr

// ptr returns the *gclink form of p.
// The result should be used for accessing fields, not stored
// in other data structures.
func (p gclinkptr) ptr() *gclink {
	return (*gclink)(unsafe.Pointer(p))
}

type stackfreelist struct {
	list gclinkptr // linked list of free stacks
	size uintptr   // total size of stacks in list
}

// dummy MSpan that contains no free objects.
var emptymspan mspan

func allocmcache() *mcache {
	lock(&mheap_.lock)
	c := (*mcache)(mheap_.cachealloc.alloc())
	unlock(&mheap_.lock)
	for i := range c.alloc {
		c.alloc[i] = &emptymspan
	}
	c.next_sample = nextSample()
	return c
}

// 返还mache的span给mcentral
func freemcache(c *mcache) {
	systemstack(func() {
		c.releaseAll()
		stackcache_clear(c)

		// NOTE(rsc,rlh): If gcworkbuffree comes back, we need to coordinate
		// with the stealing of gcworkbufs during garbage collection to avoid
		// a race where the workbuf is double-freed.
		// gcworkbuffree(c.gcworkbuf)

		lock(&mheap_.lock)
		purgecachedstats(c)
		mheap_.cachealloc.free(unsafe.Pointer(c))
		unlock(&mheap_.lock)
	})
}

// Gets a span that has a free object in it and assigns it
// to be the cached span for the given sizeclass. Returns this span.
// 指定span大小从mheap里分配一个新的span,替换当前mcache里的span
func (c *mcache) refill(spc spanClass) *mspan {
	_g_ := getg()

	_g_.m.locks++
	// Return the current cached span to the central lists.
	s := c.alloc[spc]

	if uintptr(s.allocCount) != s.nelems {
		throw("refill of span with free space remaining")
	}
	// 取消正在使用标记
	if s != &emptymspan {
		s.incache = false
	}

	// Get a new cached span from the central lists.
	// 从mheap的central里分配一个span
	s = mheap_.central[spc].mcentral.cacheSpan()
	if s == nil {
		throw("out of memory")
	}

	if uintptr(s.allocCount) == s.nelems {
		throw("span has no free space")
	}

	c.alloc[spc] = s // mheap的central里分配一个span替换了mcache里的span
	_g_.m.locks--
	return s
}

func (c *mcache) releaseAll() {
	for i := range c.alloc {
		s := c.alloc[i]
		if s != &emptymspan {
			mheap_.central[i].mcentral.uncacheSpan(s)
			c.alloc[i] = &emptymspan
		}
	}
	// Clear tinyalloc pool.
	c.tiny = 0
	c.tinyoffset = 0
}
