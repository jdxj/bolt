package bolt

import (
	"fmt"
	"sort"
	"unsafe"
)

// freelist represents a list of all pages that are available for allocation.
// It also tracks pages that have been freed but are still in use by open transactions.
//
// freelist 表示所有可分配 pages 的列表.
// 它还会跟踪已释放但仍由打开的事务使用的页面.
type freelist struct {
	ids     []pgid          // all free and available free page ids. // 所有已释放和可用的 page id.
	pending map[txid][]pgid // mapping of soon-to-be free page ids by tx. // 通过 tx 映射即将释放的 page id.
	cache   map[pgid]bool   // fast lookup of all free and pending page ids. // 快速查找所有 free 的和 pending 的 page id.
}

// newFreelist returns an empty, initialized freelist.
//
// newFreelist 返回一个空的, 初始化的 freelist.
func newFreelist() *freelist {
	return &freelist{
		pending: make(map[txid][]pgid),
		cache:   make(map[pgid]bool),
	}
}

// size returns the size of the page after serialization.
//
// size 返回序列化后 page 的大小.
func (f *freelist) size() int {
	n := f.count()
	if n >= 0xFFFF {
		// The first element will be used to store the count. See freelist.write.
		//
		// 第一个元素将用于存储计数. 参见 freelist.write.
		n++
	}
	return pageHeaderSize + (int(unsafe.Sizeof(pgid(0))) * n)
}

// count returns count of pages on the freelist
//
// count 返回 freelist 中的 page 数
func (f *freelist) count() int {
	return f.free_count() + f.pending_count()
}

// free_count returns count of free pages
func (f *freelist) free_count() int {
	return len(f.ids)
}

// pending_count returns count of pending pages
func (f *freelist) pending_count() int {
	var count int
	for _, list := range f.pending {
		count += len(list)
	}
	return count
}

// copyall copies into dst a list of all free ids and all pending ids in one sorted list.
// f.count returns the minimum length required for dst.
//
// 将所有 free ids 和所有 pending ids 的列表复制到 dst 中.
// f.count 返回 dst 所需的最小长度.
func (f *freelist) copyall(dst []pgid) {
	m := make(pgids, 0, f.pending_count())
	for _, list := range f.pending {
		m = append(m, list...)
	}
	sort.Sort(m)
	mergepgids(dst, f.ids, m)
}

// allocate returns the starting page id of a contiguous list of pages of a given size.
// If a contiguous block cannot be found then 0 is returned.
//
// allocate 返回给定大小的连续 page 列表的起始 page id.
// 如果找不到连续的块, 则返回0.
func (f *freelist) allocate(n int) pgid {
	if len(f.ids) == 0 {
		return 0
	}

	var initial, previd pgid
	for i, id := range f.ids {
		if id <= 1 {
			panic(fmt.Sprintf("invalid page allocation: %d", id))
		}

		// Reset initial page if this is not contiguous.
		if previd == 0 || id-previd != 1 {
			initial = id
		}

		// If we found a contiguous block then remove it and return it.
		if (id-initial)+1 == pgid(n) {
			// If we're allocating off the beginning then take the fast path
			// and just adjust the existing slice. This will use extra memory
			// temporarily but the append() in free() will realloc the slice
			// as is necessary.
			//
			// 如果我们要分配开始位置, 则采用快速路径并仅调整现有切片.
			// 这将临时使用额外的内存, 但是 free() 中的 append() 将根据需要重新分配切片.
			if (i + 1) == n {
				f.ids = f.ids[i+1:]
			} else {
				copy(f.ids[i-n+1:], f.ids[i+1:])
				f.ids = f.ids[:len(f.ids)-n]
			}

			// Remove from the free cache.
			for i := pgid(0); i < pgid(n); i++ {
				delete(f.cache, initial+i)
			}

			return initial
		}

		previd = id
	}
	return 0
}

// free releases a page and its overflow for a given transaction id.
// If the page is already free then a panic will occur.
//
// free 为给定的事务 id 释放 page 及其 overflow.
// 如果该 page 已经 释放, 则会发生 panic.
func (f *freelist) free(txid txid, p *page) {
	if p.id <= 1 {
		panic(fmt.Sprintf("cannot free page 0 or 1: %d", p.id))
	}

	// Free page and all its overflow pages.
	var ids = f.pending[txid]
	for id := p.id; id <= p.id+pgid(p.overflow); id++ {
		// Verify that page is not already free.
		if f.cache[id] {
			panic(fmt.Sprintf("page %d already freed", id))
		}

		// Add to the freelist and cache.
		ids = append(ids, id)
		f.cache[id] = true
	}
	f.pending[txid] = ids
}

// release moves all page ids for a transaction id (or older) to the freelist.
func (f *freelist) release(txid txid) {
	m := make(pgids, 0)
	for tid, ids := range f.pending {
		if tid <= txid {
			// Move transaction's pending pages to the available freelist.
			// Don't remove from the cache since the page is still free.
			m = append(m, ids...)
			delete(f.pending, tid)
		}
	}
	sort.Sort(m)
	f.ids = pgids(f.ids).merge(m)
}

// rollback removes the pages from a given pending tx.
func (f *freelist) rollback(txid txid) {
	// Remove page ids from cache.
	for _, id := range f.pending[txid] {
		delete(f.cache, id)
	}

	// Remove pages from pending list.
	delete(f.pending, txid)
}

// freed returns whether a given page is in the free list.
//
// freed 返回给定页面是否在 free list 中.
func (f *freelist) freed(pgid pgid) bool {
	return f.cache[pgid]
}

// read initializes the freelist from a freelist page.
//
// read 从 freelist page 初始化 freelist.
func (f *freelist) read(p *page) {
	// If the page.count is at the max uint16 value (64k) then it's considered
	// an overflow and the size of the freelist is stored as the first element.
	//
	// 如果 page.count 处于最大 uint16 值 (64k), 则将其视为溢出,
	// 并且将 freelist 的大小存储为第一个元素.
	idx, count := 0, int(p.count)
	if count == 0xFFFF {
		idx = 1
		count = int(((*[maxAllocSize]pgid)(unsafe.Pointer(&p.ptr)))[0])
	}

	// Copy the list of page ids from the freelist.
	if count == 0 {
		f.ids = nil
	} else {
		ids := ((*[maxAllocSize]pgid)(unsafe.Pointer(&p.ptr)))[idx:count]
		f.ids = make([]pgid, len(ids))
		copy(f.ids, ids)

		// Make sure they're sorted.
		sort.Sort(pgids(f.ids))
	}

	// Rebuild the page cache.
	f.reindex()
}

// write writes the page ids onto a freelist page. All free and pending ids are
// saved to disk since in the event of a program crash, all pending ids will
// become free.
//
// write 将 page ids 写入 freelist page. 所有 free 和 pending 的 ids 都保存到磁盘,
// 因为在程序崩溃时, 所有 pending ids 将变为空闲.
func (f *freelist) write(p *page) error {
	// Combine the old free pgids and pgids waiting on an open transaction.
	//
	// 合并旧的 free pgids 和等待打开事务的 pgids.

	// Update the header flag.
	p.flags |= freelistPageFlag

	// The page.count can only hold up to 64k elements so if we overflow that
	// number then we handle it by putting the size in the first element.
	//
	// page.count 最多只能容纳 64k 个元素, 因此, 如果我们溢出该数字,
	// 则可以通过将 size 放在第一个元素中来进行处理.
	lenids := f.count()
	if lenids == 0 {
		p.count = uint16(lenids)
	} else if lenids < 0xFFFF {
		p.count = uint16(lenids)
		f.copyall(((*[maxAllocSize]pgid)(unsafe.Pointer(&p.ptr)))[:])
	} else {
		p.count = 0xFFFF
		((*[maxAllocSize]pgid)(unsafe.Pointer(&p.ptr)))[0] = pgid(lenids)
		f.copyall(((*[maxAllocSize]pgid)(unsafe.Pointer(&p.ptr)))[1:])
	}

	return nil
}

// reload reads the freelist from a page and filters out pending items.
func (f *freelist) reload(p *page) {
	f.read(p)

	// Build a cache of only pending pages.
	pcache := make(map[pgid]bool)
	for _, pendingIDs := range f.pending {
		for _, pendingID := range pendingIDs {
			pcache[pendingID] = true
		}
	}

	// Check each page in the freelist and build a new available freelist
	// with any pages not in the pending lists.
	//
	// 检查 freelist 中的每个 page,
	// 并使用未在待处理列表中的任何页面构建新的可用 freelist.
	var a []pgid
	for _, id := range f.ids {
		if !pcache[id] {
			a = append(a, id)
		}
	}
	f.ids = a

	// Once the available list is rebuilt then rebuild the free cache so that
	// it includes the available and pending free pages.
	f.reindex()
}

// reindex rebuilds the free cache based on available and pending free lists.
//
// reindex 根据可用和 pending 的 free lists 重建 free caches.
func (f *freelist) reindex() {
	f.cache = make(map[pgid]bool, len(f.ids))
	for _, id := range f.ids {
		f.cache[id] = true
	}
	for _, pendingIDs := range f.pending {
		for _, pendingID := range pendingIDs {
			f.cache[pendingID] = true
		}
	}
}
