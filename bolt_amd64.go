package bolt

// maxMapSize represents the largest mmap size supported by Bolt.
const maxMapSize = 0xFFFFFFFFFFFF // 256TB

// maxAllocSize is the size used when creating array pointers.
// 二进制为31个1
const maxAllocSize = 0x7FFFFFFF

// Are unaligned load/stores broken on this arch?
//
// 未对齐的 load/stores 在此 arch 上断开了吗?
var brokenUnaligned = false
