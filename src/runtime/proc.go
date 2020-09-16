// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

var buildVersion = sys.TheVersion

// Goroutine scheduler
// The scheduler's job is to distribute ready-to-run goroutines over worker threads.
//
// The main concepts are:
// G - goroutine.
// M - worker thread, or machine.
// P - processor, a resource that is required to execute Go code.
//     M must have an associated P to execute Go code, however it can be
//     blocked or in a syscall w/o an associated P.
//
// Design doc at https://golang.org/s/go11sched.

// Worker thread parking/unparking.
// We need to balance between keeping enough running worker threads to utilize
// available hardware parallelism and parking excessive running worker threads
// to conserve CPU resources and power. This is not simple for two reasons:
// (1) scheduler state is intentionally distributed (in particular, per-P work
// queues), so it is not possible to compute global predicates on fast paths;
// (2) for optimal thread management we would need to know the future (don't park
// a worker thread when a new goroutine will be readied in near future).
//
// Three rejected approaches that would work badly:
// 1. Centralize all scheduler state (would inhibit scalability).
// 2. Direct goroutine handoff. That is, when we ready a new goroutine and there
//    is a spare P, unpark a thread and handoff it the thread and the goroutine.
//    This would lead to thread state thrashing, as the thread that readied the
//    goroutine can be out of work the very next moment, we will need to park it.
//    Also, it would destroy locality of computation as we want to preserve
//    dependent goroutines on the same thread; and introduce additional latency.
// 3. Unpark an additional thread whenever we ready a goroutine and there is an
//    idle P, but don't do handoff. This would lead to excessive thread parking/
//    unparking as the additional threads will instantly park without discovering
//    any work to do.
//
// The current approach:
// We unpark an additional thread when we ready a goroutine if (1) there is an
// idle P and there are no "spinning" worker threads. A worker thread is considered
// spinning if it is out of local work and did not find work in global run queue/
// netpoller; the spinning state is denoted in m.spinning and in sched.nmspinning.
// Threads unparked this way are also considered spinning; we don't do goroutine
// handoff so such threads are out of work initially. Spinning threads do some
// spinning looking for work in per-P run queues before parking. If a spinning
// thread finds work it takes itself out of the spinning state and proceeds to
// execution. If it does not find work it takes itself out of the spinning state
// and then parks.
// If there is at least one spinning thread (sched.nmspinning>1), we don't unpark
// new threads when readying goroutines. To compensate for that, if the last spinning
// thread finds work and stops spinning, it must unpark a new spinning thread.
// This approach smooths out unjustified spikes of thread unparking,
// but at the same time guarantees eventual maximal CPU parallelism utilization.
//
// The main implementation complication is that we need to be very careful during
// spinning->non-spinning thread transition. This transition can race with submission
// of a new goroutine, and either one part or another needs to unpark another worker
// thread. If they both fail to do that, we can end up with semi-persistent CPU
// underutilization. The general pattern for goroutine readying is: submit a goroutine
// to local work queue, #StoreLoad-style memory barrier, check sched.nmspinning.
// The general pattern for spinning->non-spinning transition is: decrement nmspinning,
// #StoreLoad-style memory barrier, check all per-P work queues for new work.
// Note that all this complexity does not apply to global run queue as we are not
// sloppy about thread unparking when submitting to global queue. Also see comments
// for nmspinning manipulation.

/*
	调度器的工作是将准备好运行的goroutine分散到工作线程中执行。
	主要的概念：
		G - gorouinte
		M - 工作线程
		P - 处理器，执行go代码的上下文，M必须绑定一个P才能执行go代码
*/

// !请注意,这大部分是google翻译的，不一定准确，因为还没看完源码，对于这段的注释并没有完全理解.
/*
我们需要在保持足够多的运行工作线程以利用可用的硬件并行性和运行过多的工作线程之间取得平衡，
以节省CPU资源和电源。以下两个原因使得达到这个目标并不简单：
（1）调度程序状态是故意分布的（特别是每个P工作队列），因此不可能在快速路径上计算出全局的结果;
（2）为了获得最佳的线程管理，我们需要知道未来（当不久的将来准备新的goroutine时，不要park工作线程）。

三种实现方法会很糟糕：
1.集中所有调度程序状态（会抑制可伸缩性）。

2.直接goroutine切换。也就是说，当我们准备一个新的goroutine并且有一个备用的P时
，取消park线程并将其交给线程和goroutine。这将导致线程状态颠簸，
因为准备goroutine的线程可能在下一刻失去工作，我们将需要park它。此外，
它会破坏计算的位置，因为我们想在同一个线程上保留依赖的goroutine;并引入额外的延迟。

3.每当我们准备一个goroutine并且有一个空闲的P但是不进行切换时，
取消park一个额外的线程。这将导致过多的线程parking/unparking，
因为额外的线程将立即park而发现不了任何工作要做。

当前的实现方法：

当我们准备调度一个goroutine时，
如果有一个空闲的P并且没有“自旋”的工作线程，我们取消另外一个线程。
如果工作线程在本地队列、全局运行队列、netpoller中找不到工作，则认为它正在自旋;
自旋状态用m.spinning和sched.nmspinning表示。以这种方式park的线程也被认为是自旋的;
因为这样的线程已经失去了工作，我们不做goroutine切换。
在parking前，自旋线程会在每个P运行队列中进行一些自旋工作。
如果自旋线程找到工作，它将自己从自旋状态中移出并继续执行。
如果找不到工作，它会自动退出自旋状态，然后parks。

如果至少有一个自旋线程（sched.nmspinning>1），我们在准备goroutines时不会取消park新线程。
为了弥补这一点，如果最后一个自旋线程找到工作并停止自旋，
它必须取消park一个新的自旋线程。这种方法可以消除线程取消停顿的不合理峰值，
但同时保证最终的最大CPU并行度利用率。主要的实现复杂性是在自旋 - >非自旋线程转换期间我们需要非常小心。
这种转变可以在提交新的goroutine时竞争，并且任何一个部分都需要取消另一个工作线程。
如果他们都没有做到这一点，我们最终可能会出现半持久性CPU未充分利用。

goroutine准备的一般模式是：
将goroutine提交到本地工作队列，＃StoreLoad样式的内存屏障，检查sched.nmspinning。
自旋 - >非自旋过渡的一般模式是：减少nmspinning，
＃StoreLoad样式的内存屏障，检查所有每个P工作队列的新工作。
请注意，所有这些复杂性都不适用于全局运行队列，
因为我们在提交到全局队列时并不松散于线程解除挂起。
另请参阅有关 nmspinning 操作的注释。
*/

var (
	// 这里的全局 m0 和 g0 是比较特殊的存在
	// m0 表示进程启动的第一个线程，也叫主线程
	// g0 表示和 m0 绑定的 groutine，也是第一个 groutine，
	// g0 上的栈是系统分配的栈，它仅用于负责调度的G, 且不指向任何可执行的函数，
	// 但要注意的是每个m都会有一个自己的g0，而不仅仅是 m0 有 g0。
	// 在调度或系统调用时会使用 g0 的栈空间
	m0           m
	g0           g
	raceprocctx0 uintptr
)

//go:linkname runtime_init runtime.init
func runtime_init()

//go:linkname main_init main.init
func main_init()

// main_init_done is a signal used by cgocallbackg that initialization
// has been completed. It is made before _cgo_notify_runtime_init_done,
// so all cgo calls can rely on it existing. When main_init is complete,
// it is closed, meaning cgocallbackg can reliably receive from it.
var main_init_done chan bool

//go:linkname main_main main.main
func main_main()

// mainStarted indicates that the main M has started.
var mainStarted bool

// runtimeInitTime is the nanotime() at which the runtime started.
var runtimeInitTime int64

// Value to use for signal mask for newly created M's.
var initSigmask sigset

// 由 asm_amd64.s 中 runtime·rt0_go来调用，为真正的main函数准备
// The main goroutine.
func main() {
	println("in main.....")
	// 获取 main goroutine
	g := getg()

	// Racectx of m0->g0 is used only as the parent of the main goroutine.
	// It must not be used for anything else.
	g.m.g0.racectx = 0

	// Max stack size is 1 GB on 64-bit, 250 MB on 32-bit.
	// Using decimal instead of binary GB and MB because
	// they look nicer in the stack overflow failure message.
	// 设置最大的栈大小
	if sys.PtrSize == 8 {
		maxstacksize = 1000000000
	} else {
		maxstacksize = 250000000
	}

	// Allow newproc to start new Ms.
	// 允许 newproc 启动 Ms
	mainStarted = true

	// 在系统栈上运行 sysmon
	systemstack(func() {
		// 分配一个新的m，运行sysmon系统后台监控（定期垃圾回收和调度抢占）
		newm(sysmon, nil)
	})

	// Lock the main goroutine onto this, the main OS thread,
	// during initialization. Most programs won't care, but a few
	// do require certain calls to be made by the main thread.
	// Those can arrange for main.main to run in the main thread
	// by calling runtime.LockOSThread during initialization
	// to preserve the lock.
	// main goroutine会绑定一个M
	lockOSThread()

	// 确保是主线程
	if g.m != &m0 {
		throw("runtime.main not on m0")
	}

	// runtime 内部 init 函数的执行，编译器动态生成的。
	runtime_init() // must be before defer
	// 如果获取系统时间的ns为0，直接panic
	if nanotime() == 0 {
		throw("nanotime returning zero")
	}

	// Defer unlock so that runtime.Goexit during init does the unlock too.
	needUnlock := true
	defer func() {
		if needUnlock {
			unlockOSThread()
		}
	}()

	// Record when the world started. Must be after runtime_init
	// because nanotime on some platforms depends on startNano.
	runtimeInitTime = nanotime()

	// gc 启动一个goroutine进行gc清扫
	gcenable()

	main_init_done = make(chan bool)
	if iscgo {
		if _cgo_thread_start == nil {
			throw("_cgo_thread_start missing")
		}
		if GOOS != "windows" {
			if _cgo_setenv == nil {
				throw("_cgo_setenv missing")
			}
			if _cgo_unsetenv == nil {
				throw("_cgo_unsetenv missing")
			}
		}
		if _cgo_notify_runtime_init_done == nil {
			throw("_cgo_notify_runtime_init_done missing")
		}
		// Start the template thread in case we enter Go from
		// a C-created thread and need to create a new thread.
		startTemplateThread()
		cgocall(_cgo_notify_runtime_init_done, nil)
	}

	// 执行init函数，编译器动态生成的，包括用户定义的所有的init函数。
	fn := main_init // make an indirect call, as the linker doesn't know the address of the main package when laying down the runtime
	fn()
	close(main_init_done)

	needUnlock = false
	// 到这里的时候解除main g绑定m
	unlockOSThread()

	// -buildmode=c-archive or c-shared 把go编译成静态或者动态库
	if isarchive || islibrary {
		// A program compiled with -buildmode=c-archive or c-shared
		// has a main, but it is not executed.
		return
	}

	// 真正的执行main func in package main
	fn = main_main // make an indirect call, as the linker doesn't know the address of the main package when laying down the runtime
	fn()

	// -race
	if raceenabled {
		racefini()
	}

	// Make racy client program work: if panicking on
	// another goroutine at the same time as main returns,
	// let the other goroutine finish printing the panic trace.
	// Once it does, it will exit. See issues 3934 and 20018.
	// 到这里main func已经执行完了，如果其他的goroutine有panic，
	// 让panic 信息打印出来
	if atomic.Load(&runningPanicDefers) != 0 {
		// Running deferred functions should not take long.
		for c := 0; c < 1000; c++ {
			if atomic.Load(&runningPanicDefers) == 0 {
				break
			}
			Gosched()
		}
	}
	if atomic.Load(&panicking) != 0 {
		gopark(nil, nil, "panicwait", traceEvGoStop, 1)
	}
	// 退出程序
	exit(0)

	// ? 为何这里还需要for循环？
	// 下面的for循环一定会导致程序崩掉，这样就确保了程序一定会退出
	for {
		var x *int32
		*x = 0
	}
}

// os_beforeExit is called from os.Exit(0).
//go:linkname os_beforeExit os.runtime_beforeExit
// 由 os.Exit(0) 函数调用，会被编译器链接成 os.runtime_beforeExit
func os_beforeExit() {
	if raceenabled {
		racefini()
	}
}

// start forcegc helper goroutine
// 初始化 forcegchelper goroutine
// 用来执行强制gc
func init() {
	go forcegchelper()
}

// 用来执行强制gc
func forcegchelper() {
	forcegc.g = getg()
	for {
		lock(&forcegc.lock)
		if forcegc.idle != 0 {
			throw("forcegc: phase error")
		}
		atomic.Store(&forcegc.idle, 1)
		goparkunlock(&forcegc.lock, "force gc (idle)", traceEvGoBlock, 1)
		// this goroutine is explicitly resumed by sysmon
		if debug.gctrace > 0 {
			println("GC forced")
		}
		// Time-triggered, fully concurrent.
		gcStart(gcBackgroundMode, gcTrigger{kind: gcTriggerTime, now: nanotime()})
	}
}

//go:nosplit

// Gosched yields the processor, allowing other goroutines to run. It does not
// suspend the current goroutine, so execution resumes automatically.
// Gosched 让出当前的P，允许其他的goroutine去运行。
// 但是并不假设当前的G一定立马就运行
// 因为gosched_m操作就是让当前的G加入到全局队列中，
// 然后触发调度
func Gosched() {
	mcall(gosched_m)
}

// goschedguarded yields the processor like gosched, but also checks
// for forbidden states and opts out of the yield in those cases.
//go:nosplit
// goschedguarded 会像处理 gosched 一样让出处理器，但也检查禁止状态并在这些情况下选择退让。
func goschedguarded() {
	mcall(goschedguarded_m)
}

// Puts the current goroutine into a waiting state and calls unlockf.
// If unlockf returns false, the goroutine is resumed.
// unlockf must not access this G's stack, as it may be moved between
// the call to gopark and the call to unlockf.
//
// channel、Go语言的定时器、网络IO操作都会调用 gopark。
// unlockf 解锁函数指针，lock Lock 指针，reason park 的原因
// 暂停当前G的运行，用 unlockf 作为判断是否恢复运行，所以该G必须主动恢复，
// 否则该任务会遗失。
func gopark(unlockf func(*g, unsafe.Pointer) bool, lock unsafe.Pointer, reason string, traceEv byte, traceskip int) {
	mp := acquirem()
	gp := mp.curg
	status := readgstatus(gp)
	// 当前的g必须正在运行
	if status != _Grunning && status != _Gscanrunning {
		throw("gopark: bad g status")
	}
	mp.waitlock = lock
	mp.waitunlockf = *(*unsafe.Pointer)(unsafe.Pointer(&unlockf))
	gp.waitreason = reason
	mp.waittraceev = traceEv
	mp.waittraceskip = traceskip
	releasem(mp)
	// can't do anything that might move the G between Ms here.
	mcall(park_m)
}

// Puts the current goroutine into a waiting state and unlocks the lock.
// The goroutine can be made runnable again by calling goready(gp).
// 将当前 goroutine 置于等待状态并解锁锁定。
// 通过调用 goready（gp）可以使 goroutine 再次运行。
func goparkunlock(lock *mutex, reason string, traceEv byte, traceskip int) {
	gopark(parkunlock_c, unsafe.Pointer(lock), reason, traceEv, traceskip)
}

// 与gopark配套，用于恢复G
func goready(gp *g, traceskip int) {
	systemstack(func() {
		ready(gp, traceskip, true)
	})
}

//go:nosplit
// 从 p 的 sugog 缓存中获取一个 sudog
func acquireSudog() *sudog {
	// Delicate dance: the semaphore implementation calls
	// acquireSudog, acquireSudog calls new(sudog),
	// new calls malloc, malloc can call the garbage collector,
	// and the garbage collector calls the semaphore implementation
	// in stopTheWorld.
	// Break the cycle by doing acquirem/releasem around new(sudog).
	// The acquirem/releasem increments m.locks during new(sudog),
	// which keeps the garbage collector from being invoked.
	// 信号量实现调用acquireSudog，acquireSudog调用new（sudog），
	// 新调用malloc，malloc可以调用垃圾收集器，垃圾收集器调用stopTheWorld中的信号量实现。
	// 通过在new（sudog）周围执行acquirem/releasem来打破循环。在新的（sudog）期间，
	// acquirem/releasem增加m.locks，这使得垃圾收集器不会被调用。
	mp := acquirem()
	pp := mp.p.ptr()
	if len(pp.sudogcache) == 0 {
		lock(&sched.sudoglock)
		// First, try to grab a batch from central cache.
		for len(pp.sudogcache) < cap(pp.sudogcache)/2 && sched.sudogcache != nil {
			s := sched.sudogcache
			sched.sudogcache = s.next
			s.next = nil
			pp.sudogcache = append(pp.sudogcache, s)
		}
		unlock(&sched.sudoglock)
		// If the central cache is empty, allocate a new one.
		if len(pp.sudogcache) == 0 {
			pp.sudogcache = append(pp.sudogcache, new(sudog))
		}
	}
	n := len(pp.sudogcache)
	s := pp.sudogcache[n-1]
	pp.sudogcache[n-1] = nil
	pp.sudogcache = pp.sudogcache[:n-1]
	if s.elem != nil {
		throw("acquireSudog: found s.elem != nil in cache")
	}
	releasem(mp)
	return s
}

//go:nosplit
func releaseSudog(s *sudog) {
	if s.elem != nil {
		throw("runtime: sudog with non-nil elem")
	}
	if s.isSelect {
		throw("runtime: sudog with non-false isSelect")
	}
	if s.next != nil {
		throw("runtime: sudog with non-nil next")
	}
	if s.prev != nil {
		throw("runtime: sudog with non-nil prev")
	}
	if s.waitlink != nil {
		throw("runtime: sudog with non-nil waitlink")
	}
	if s.c != nil {
		throw("runtime: sudog with non-nil c")
	}
	gp := getg()
	if gp.param != nil {
		throw("runtime: releaseSudog with non-nil gp.param")
	}
	mp := acquirem() // avoid rescheduling to another P
	pp := mp.p.ptr()
	if len(pp.sudogcache) == cap(pp.sudogcache) {
		// Transfer half of local cache to the central cache.
		var first, last *sudog
		for len(pp.sudogcache) > cap(pp.sudogcache)/2 {
			n := len(pp.sudogcache)
			p := pp.sudogcache[n-1]
			pp.sudogcache[n-1] = nil
			pp.sudogcache = pp.sudogcache[:n-1]
			if first == nil {
				first = p
			} else {
				last.next = p
			}
			last = p
		}
		lock(&sched.sudoglock)
		last.next = sched.sudogcache
		sched.sudogcache = first
		unlock(&sched.sudoglock)
	}
	pp.sudogcache = append(pp.sudogcache, s)
	releasem(mp)
}

// funcPC returns the entry PC of the function f.
// It assumes that f is a func value. Otherwise the behavior is undefined.
// CAREFUL: In programs with plugins, funcPC can return different values
// for the same function (because there are actually multiple copies of
// the same function in the address space). To be safe, don't use the
// results of this function in any == expression. It is only safe to
// use the result as an address at which to start executing code.
// funcPC returns the entry PC of the function f.
//
// funcPC返回函数f的条目PC。它假设f是一个函数值。否则行为未定义。
// 小心：在带插件的程序中，funcPC可以为同一个函数返回不同的值（因为实际上在地址空间中有相同函数的多个副本）。
// 为安全起见，请勿在任何==表达式中使用此函数的结果。将结果用作开始执行代码的地址是安全的。
//go:nosplit
// 得到go函数f的执行地址
// 以下来自微信讨论，感谢 @frank-hust 网友的精彩分析
// 首先f是个 eface，其结构如下 runtime2.go,
// type eface struct {
// 	_type *_type
// 	data  unsafe.Pointer
// }
// &f 得到 eface 的地址，然后加上 sys.PtrSize 就是得到f这个 eface 里data字段，
// 而对于传入一个函数来说, data是一个指向某个函数名的指针.
// 所以这是一个指向指针的指针. 要取函数位置的话, 用两个**。
// 为啥要用interface多一层转换, 因为不同的函数类型不同, 所以得用interface
/*
+-----+
|_type|
+-----+      +-----+      +------+
|data | ---->|f ptr| ---->|f addr|
+-----+      +-----+      +------+

`f ptr`指向函数起始地址的指针
*/
func funcPC(f interface{}) uintptr {
	return **(**uintptr)(add(unsafe.Pointer(&f), sys.PtrSize))
}

// called from assembly
func badmcall(fn func(*g)) {
	throw("runtime: mcall called on m->g0 stack")
}

func badmcall2(fn func(*g)) {
	throw("runtime: mcall function returned")
}

func badreflectcall() {
	panic(plainError("arg size to reflect.call more than 1GB"))
}

var badmorestackg0Msg = "fatal: morestack on g0\n"

//go:nosplit
//go:nowritebarrierrec
func badmorestackg0() {
	sp := stringStructOf(&badmorestackg0Msg)
	write(2, sp.str, int32(sp.len))
}

var badmorestackgsignalMsg = "fatal: morestack on gsignal\n"

//go:nosplit
//go:nowritebarrierrec
func badmorestackgsignal() {
	sp := stringStructOf(&badmorestackgsignalMsg)
	write(2, sp.str, int32(sp.len))
}

//go:nosplit
func badctxt() {
	throw("ctxt != 0")
}

func lockedOSThread() bool {
	gp := getg()
	return gp.lockedm != 0 && gp.m.lockedg != 0
}

var (
	// 存储所有g的数组
	allgs []*g
	// 保护allgs的互斥锁
	allglock mutex
)

func allgadd(gp *g) {
	if readgstatus(gp) == _Gidle {
		throw("allgadd: bad status Gidle")
	}

	lock(&allglock)
	allgs = append(allgs, gp)
	allglen = uintptr(len(allgs))
	unlock(&allglock)
}

const (
	// Number of goroutine ids to grab from sched.goidgen to local per-P cache at once.
	// 16 seems to provide enough amortization, but other than that it's mostly arbitrary number.
	_GoidCacheBatch = 16
)

// The bootstrap sequence is:
//
//	call osinit
//	call schedinit
//	make & queue new G
//	call runtime·mstart
//
// The new G calls runtime·main.
// 调度系统的初始化
// 也包含了gc的初始化
func schedinit() {
	// raceinit must be the first call to race detector.
	// In particular, it must be done before mallocinit below calls racemapshadow.
	// 获取当前的G，然后进行race init
	// ? 这里有疑问？在调用schedinit之前应该没有生成G，谈何getg()？
	// 有的，是g0和m0，但这个时候没有p是真的
	_g_ := getg()
	// 检查编译时是否开启了-race
	if raceenabled {
		_g_.racectx, raceprocctx0 = raceinit()
	}

	// 设置m的最大值为10000
	sched.maxmcount = 10000

	tracebackinit()
	moduledataverify()
	// 栈的初始化
	stackinit()
	// 内存分配初始化
	mallocinit()
	mcommoninit(_g_.m)

	alginit() // maps must not be used before this call
	// plugin 相关的初始化
	modulesinit()   // provides activeModules
	typelinksinit() // uses maps, activeModules
	itabsinit()     // uses activeModules

	msigsave(_g_.m)
	initSigmask = _g_.m.sigmask

	// 处理命令行
	goargs()
	// 处理环境变量
	goenvs()
	// 处理一些用于debug的参数
	// 如： GODEBUG=schedtrace=1000
	parsedebugvars()

	// gc初始化
	gcinit()

	sched.lastpoll = uint64(nanotime())
	// 确认P的个数
	// 默认等于cpu个数，可以通过GOMAXPROCS环境变量更改
	procs := ncpu
	if n, ok := atoi32(gogetenv("GOMAXPROCS")); ok && n > 0 {
		procs = n
	}
	// 调整P的个数，这里是新分配procs个P
	// 这个函数很重要，所有的P都是从这里分配的，以后也不用担心没有P了
	if procresize(procs) != nil {
		throw("unknown runnable goroutine during bootstrap")
	}

	// For cgocheck > 1, we turn on the write barrier at all times
	// and check all pointer writes. We can't do this until after
	// procresize because the write barrier needs a P.
	if debug.cgocheck > 1 {
		writeBarrier.cgo = true
		writeBarrier.enabled = true
		for _, p := range allp {
			p.wbBuf.reset()
		}
	}

	if buildVersion == "" {
		// Condition should never trigger. This code just serves
		// to ensure runtime·buildVersion is kept in the resulting binary.
		buildVersion = "unknown"
	}
}

func dumpgstatus(gp *g) {
	_g_ := getg()
	print("runtime: gp: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
	print("runtime:  g:  g=", _g_, ", goid=", _g_.goid, ",  g->atomicstatus=", readgstatus(_g_), "\n")
}

func checkmcount() {
	// sched lock is held
	if mcount() > sched.maxmcount {
		print("runtime: program exceeds ", sched.maxmcount, "-thread limit\n")
		throw("thread exhaustion")
	}
}

func mcommoninit(mp *m) {
	_g_ := getg()

	// g0 stack won't make sense for user (and is not necessary unwindable).
	if _g_ != _g_.m.g0 {
		callers(1, mp.createstack[:])
	}

	lock(&sched.lock)
	if sched.mnext+1 < sched.mnext {
		throw("runtime: thread ID overflow")
	}
	mp.id = sched.mnext
	sched.mnext++
	checkmcount()

	mp.fastrand[0] = 1597334677 * uint32(mp.id)
	mp.fastrand[1] = uint32(cputicks())
	if mp.fastrand[0]|mp.fastrand[1] == 0 {
		mp.fastrand[1] = 1
	}

	mpreinit(mp)
	if mp.gsignal != nil {
		mp.gsignal.stackguard1 = mp.gsignal.stack.lo + _StackGuard
	}

	// Add to allm so garbage collector doesn't free g->m
	// when it is just in a register or thread-local storage.
	mp.alllink = allm

	// NumCgoCall() iterates over allm w/o schedlock,
	// so we need to publish it safely.
	atomicstorep(unsafe.Pointer(&allm), unsafe.Pointer(mp))
	unlock(&sched.lock)

	// Allocate memory to hold a cgo traceback if the cgo call crashes.
	if iscgo || GOOS == "solaris" || GOOS == "windows" {
		mp.cgoCallers = new(cgoCallers)
	}
}

// Mark gp ready to run.
// 将gp的状态更改为_Grunnable，以便调度器调度执行
// 并且如果next==true，那么设置为优先级最高，并尝试wakep
func ready(gp *g, traceskip int, next bool) {
	if trace.enabled {
		traceGoUnpark(gp, traceskip)
	}

	status := readgstatus(gp)

	// Mark runnable.
	_g_ := getg()
	_g_.m.locks++ // disable preemption because it can be holding p in a local var
	if status&^_Gscan != _Gwaiting {
		dumpgstatus(gp)
		throw("bad g->status in ready")
	}

	// status is Gwaiting or Gscanwaiting, make Grunnable and put on runq
	casgstatus(gp, _Gwaiting, _Grunnable)
	runqput(_g_.m.p.ptr(), gp, next)
	// 如果有空闲P且没有自旋的M。
	if atomic.Load(&sched.npidle) != 0 && atomic.Load(&sched.nmspinning) == 0 {
		wakep()
	}
	_g_.m.locks--
	if _g_.m.locks == 0 && _g_.preempt { // restore the preemption request in Case we've cleared it in newstack
		_g_.stackguard0 = stackPreempt
	}
}

func gcprocs() int32 {
	// Figure out how many CPUs to use during GC.
	// Limited by gomaxprocs, number of actual CPUs, and MaxGcproc.
	lock(&sched.lock)
	n := gomaxprocs
	if n > ncpu {
		n = ncpu
	}
	if n > _MaxGcproc {
		n = _MaxGcproc
	}
	if n > sched.nmidle+1 { // one M is currently running
		n = sched.nmidle + 1
	}
	unlock(&sched.lock)
	return n
}

func needaddgcproc() bool {
	lock(&sched.lock)
	n := gomaxprocs
	if n > ncpu {
		n = ncpu
	}
	if n > _MaxGcproc {
		n = _MaxGcproc
	}
	n -= sched.nmidle + 1 // one M is currently running
	unlock(&sched.lock)
	return n > 0
}

func helpgc(nproc int32) {
	_g_ := getg()
	lock(&sched.lock)
	pos := 0
	for n := int32(1); n < nproc; n++ { // one M is currently running
		if allp[pos].mcache == _g_.m.mcache {
			pos++
		}
		mp := mget()
		if mp == nil {
			throw("gcprocs inconsistency")
		}
		mp.helpgc = n
		mp.p.set(allp[pos])
		mp.mcache = allp[pos].mcache
		pos++
		notewakeup(&mp.park)
	}
	unlock(&sched.lock)
}

// freezeStopWait is a large value that freezetheworld sets
// sched.stopwait to in order to request that all Gs permanently stop.
const freezeStopWait = 0x7fffffff

// freezing is set to non-zero if the runtime is trying to freeze the
// world.
var freezing uint32

// Similar to stopTheWorld but best-effort and can be called several times.
// There is no reverse operation, used during crashing.
// This function must not lock any mutexes.
func freezetheworld() {
	atomic.Store(&freezing, 1)
	// stopwait and preemption requests can be lost
	// due to races with concurrently executing threads,
	// so try several times
	for i := 0; i < 5; i++ {
		// this should tell the scheduler to not start any new goroutines
		sched.stopwait = freezeStopWait
		atomic.Store(&sched.gcwaiting, 1)
		// this should stop running goroutines
		if !preemptall() {
			break // no running goroutines
		}
		usleep(1000)
	}
	// to be sure
	usleep(1000)
	preemptall()
	usleep(1000)
}

func isscanstatus(status uint32) bool {
	if status == _Gscan {
		throw("isscanstatus: Bad status Gscan")
	}
	return status&_Gscan == _Gscan
}

// All reads and writes of g's status go through readgstatus, casgstatus
// castogscanstatus, casfrom_Gscanstatus.
//go:nosplit
func readgstatus(gp *g) uint32 {
	return atomic.Load(&gp.atomicstatus)
}

// Ownership of gcscanvalid:
//
// If gp is running (meaning status == _Grunning or _Grunning|_Gscan),
// then gp owns gp.gcscanvalid, and other goroutines must not modify it.
//
// Otherwise, a second goroutine can lock the scan state by setting _Gscan
// in the status bit and then modify gcscanvalid, and then unlock the scan state.
//
// Note that the first condition implies an exception to the second:
// if a second goroutine changes gp's status to _Grunning|_Gscan,
// that second goroutine still does not have the right to modify gcscanvalid.

// The Gscanstatuses are acting like locks and this releases them.
// If it proves to be a performance hit we should be able to make these
// simple atomic stores but for now we are going to throw if
// we see an inconsistent state.
func casfrom_Gscanstatus(gp *g, oldval, newval uint32) {
	success := false

	// Check that transition is valid.
	switch oldval {
	default:
		print("runtime: casfrom_Gscanstatus bad oldval gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
		dumpgstatus(gp)
		throw("casfrom_Gscanstatus:top gp->status is not in scan state")
	case _Gscanrunnable,
		_Gscanwaiting,
		_Gscanrunning,
		_Gscansyscall:
		if newval == oldval&^_Gscan {
			success = atomic.Cas(&gp.atomicstatus, oldval, newval)
		}
	}
	if !success {
		print("runtime: casfrom_Gscanstatus failed gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
		dumpgstatus(gp)
		throw("casfrom_Gscanstatus: gp->status is not in scan state")
	}
}

// This will return false if the gp is not in the expected status and the cas fails.
// This acts like a lock acquire while the casfromgstatus acts like a lock release.
func castogscanstatus(gp *g, oldval, newval uint32) bool {
	switch oldval {
	case _Grunnable,
		_Grunning,
		_Gwaiting,
		_Gsyscall:
		if newval == oldval|_Gscan {
			return atomic.Cas(&gp.atomicstatus, oldval, newval)
		}
	}
	print("runtime: castogscanstatus oldval=", hex(oldval), " newval=", hex(newval), "\n")
	throw("castogscanstatus")
	panic("not reached")
}

// If asked to move to or from a Gscanstatus this will throw. Use the castogscanstatus
// and casfrom_Gscanstatus instead.
// casgstatus will loop if the g->atomicstatus is in a Gscan status until the routine that
// put it in the Gscan state is finished.
//go:nosplit
func casgstatus(gp *g, oldval, newval uint32) {
	if (oldval&_Gscan != 0) || (newval&_Gscan != 0) || oldval == newval {
		systemstack(func() {
			print("runtime: casgstatus: oldval=", hex(oldval), " newval=", hex(newval), "\n")
			throw("casgstatus: bad incoming values")
		})
	}

	if oldval == _Grunning && gp.gcscanvalid {
		// If oldvall == _Grunning, then the actual status must be
		// _Grunning or _Grunning|_Gscan; either way,
		// we own gp.gcscanvalid, so it's safe to read.
		// gp.gcscanvalid must not be true when we are running.
		systemstack(func() {
			print("runtime: casgstatus ", hex(oldval), "->", hex(newval), " gp.status=", hex(gp.atomicstatus), " gp.gcscanvalid=true\n")
			throw("casgstatus")
		})
	}

	// See http://golang.org/cl/21503 for justification of the yield delay.
	const yieldDelay = 5 * 1000
	var nextYield int64

	// loop if gp->atomicstatus is in a scan state giving
	// GC time to finish and change the state to oldval.
	for i := 0; !atomic.Cas(&gp.atomicstatus, oldval, newval); i++ {
		if oldval == _Gwaiting && gp.atomicstatus == _Grunnable {
			systemstack(func() {
				throw("casgstatus: waiting for Gwaiting but is Grunnable")
			})
		}
		// Help GC if needed.
		// if gp.preemptscan && !gp.gcworkdone && (oldval == _Grunning || oldval == _Gsyscall) {
		// 	gp.preemptscan = false
		// 	systemstack(func() {
		// 		gcphasework(gp)
		// 	})
		// }
		// But meanwhile just yield.
		if i == 0 {
			nextYield = nanotime() + yieldDelay
		}
		if nanotime() < nextYield {
			for x := 0; x < 10 && gp.atomicstatus != oldval; x++ {
				procyield(1)
			}
		} else {
			osyield()
			nextYield = nanotime() + yieldDelay/2
		}
	}
	if newval == _Grunning {
		gp.gcscanvalid = false
	}
}

// casgstatus(gp, oldstatus, Gcopystack), assuming oldstatus is Gwaiting or Grunnable.
// Returns old status. Cannot call casgstatus directly, because we are racing with an
// async wakeup that might come in from netpoll. If we see Gwaiting from the readgstatus,
// it might have become Grunnable by the time we get to the cas. If we called casgstatus,
// it would loop waiting for the status to go back to Gwaiting, which it never will.
// casgstatus(gp, oldstatus, Gcopystack)，假设oldstatus是Gwaiting或Grunnable。返回旧的状态。
// 不能直接调用casgstatus，因为我们正在与可能来自netpoll的异步唤醒竞争。如果我们从readgstatus看到Gwaiting，
// 那么当我们到达cas时，它可能已经变成Grunnable了。如果我们调用casgstatus，它将循环等待状态返回Gwaiting，它永远不会这样做。
//go:nosplit
func casgcopystack(gp *g) uint32 {
	for {
		oldstatus := readgstatus(gp) &^ _Gscan
		if oldstatus != _Gwaiting && oldstatus != _Grunnable {
			throw("copystack: bad status, not Gwaiting or Grunnable")
		}
		if atomic.Cas(&gp.atomicstatus, oldstatus, _Gcopystack) {
			return oldstatus
		}
	}
}

// scang blocks until gp's stack has been scanned.
// It might be scanned by scang or it might be scanned by the goroutine itself.
// Either way, the stack scan has completed when scang returns.
// scang会一直阻塞，直到g上的栈扫描完
// scang有可能是自己扫描自己
func scang(gp *g, gcw *gcWork) {
	// Invariant; we (the caller, markroot for a specific goroutine) own gp.gcscandone.
	// Nothing is racing with us now, but gcscandone might be set to true left over
	// from an earlier round of stack scanning (we scan twice per GC).
	// We use gcscandone to record whether the scan has been done during this round.

	// 标记扫描未完成
	gp.gcscandone = false

	// See http://golang.org/cl/21503 for justification of the yield delay.
	const yieldDelay = 10 * 1000
	var nextYield int64

	// Endeavor to get gcscandone set to true,
	// either by doing the stack scan ourselves or by coercing gp to scan itself.
	// gp.gcscandone can transition from false to true when we're not looking
	// (if we asked for preemption), so any time we lock the status using
	// castogscanstatus we have to double-check that the scan is still not done.
	// 循环直到扫描完成
loop:
	for i := 0; !gp.gcscandone; i++ {
		// 查看当前G的状态
		switch s := readgstatus(gp); s {
		default:
			// 都不是下面的状态抛出异常
			dumpgstatus(gp)
			throw("stopg: invalid status")

		case _Gdead:
			// G已经结束，不需要扫描G栈
			// No stack.
			gp.gcscandone = true
			break loop

		case _Gcopystack:
		// G的栈正在扩展, 下一轮重试
		// Stack being switched. Go around again.

		case _Grunnable, _Gsyscall, _Gwaiting:
			// Claim goroutine by setting scan bit.
			// Racing with execution or readying of gp.
			// The scan bit keeps them from running
			// the goroutine until we're done.
			// G不是正在运行中, 更改G的状态，防止它运行
			if castogscanstatus(gp, s, s|_Gscan) {
				if !gp.gcscandone {
					// 扫描G上的栈
					scanstack(gp, gcw)
					gp.gcscandone = true
				}
				// 恢复G的状态, 并跳出循环
				restartg(gp)
				break loop
			}

		case _Gscanwaiting:
		// G正在扫描它自己, 等待扫描完毕
		// newstack is doing a scan for us right now. Wait.

		case _Grunning:
			// G正在运行
			// Goroutine running. Try to preempt execution so it can scan itself.
			// The preemption handler (in newstack) does the actual scan.

			// Optimization: if there is already a pending preemption request
			// (from the previous loop iteration), don't bother with the atomics.
			// 如果已经有抢占请求, 则抢占成功时会帮我们处理
			if gp.preemptscan && gp.preempt && gp.stackguard0 == stackPreempt {
				break
			}

			// Ask for preemption and self scan.
			// 抢占G, 抢占成功时G会扫描它自己
			if castogscanstatus(gp, _Grunning, _Gscanrunning) {
				if !gp.gcscandone {
					// 设置扫描可抢占
					gp.preemptscan = true
					// 设置可抢占
					gp.preempt = true
					gp.stackguard0 = stackPreempt
				}
				casfrom_Gscanstatus(gp, _Gscanrunning, _Grunning)
			}
		}
		//  第一轮休眠10毫秒, 第二轮休眠5毫秒
		if i == 0 {
			nextYield = nanotime() + yieldDelay
		}
		if nanotime() < nextYield {
			procyield(10)
		} else {
			osyield()
			nextYield = nanotime() + yieldDelay/2
		}
	}

	// 扫描完成, 取消抢占扫描的请求
	gp.preemptscan = false // cancel scan request if no longer needed
}

// The GC requests that this routine be moved from a scanmumble state to a mumble state.
func restartg(gp *g) {
	s := readgstatus(gp)
	switch s {
	default:
		dumpgstatus(gp)
		throw("restartg: unexpected status")

	case _Gdead:
	// ok

	case _Gscanrunnable,
		_Gscanwaiting,
		_Gscansyscall:
		casfrom_Gscanstatus(gp, s, s&^_Gscan)
	}
}

// stopTheWorld stops all P's from executing goroutines, interrupting
// all goroutines at GC safe points and records reason as the reason
// for the stop. On return, only the current goroutine's P is running.
// stopTheWorld must not be called from a system stack and the caller
// must not hold worldsema. The caller must call startTheWorld when
// other P's should resume execution.
//
// stopTheWorld is safe for multiple goroutines to call at the
// same time. Each will execute its own stop, and the stops will
// be serialized.
//
// This is also used by routines that do stack dumps. If the system is
// in panic or being exited, this may not reliably stop all
// goroutines.
func stopTheWorld(reason string) {
	semacquire(&worldsema)
	getg().m.preemptoff = reason
	systemstack(stopTheWorldWithSema)
}

// startTheWorld undoes the effects of stopTheWorld.
// startTheWorld 启动世界
func startTheWorld() {
	systemstack(func() { startTheWorldWithSema(false) })
	// worldsema must be held over startTheWorldWithSema to ensure
	// gomaxprocs cannot change while worldsema is held.
	semrelease(&worldsema)
	getg().m.preemptoff = ""
}

// Holding worldsema grants an M the right to try to stop the world
// and prevents gomaxprocs from changing concurrently.
var worldsema uint32 = 1

// stopTheWorldWithSema is the core implementation of stopTheWorld.
// The caller is responsible for acquiring worldsema and disabling
// preemption first and then should stopTheWorldWithSema on the system
// stack:
//
//	semacquire(&worldsema, 0)
//	m.preemptoff = "reason"
//	systemstack(stopTheWorldWithSema)
//
// When finished, the caller must either call startTheWorld or undo
// these three operations separately:
//
//	m.preemptoff = ""
//	systemstack(startTheWorldWithSema)
//	semrelease(&worldsema)
//
// It is allowed to acquire worldsema once and then execute multiple
// startTheWorldWithSema/stopTheWorldWithSema pairs.
// Other P's are able to execute between successive calls to
// startTheWorldWithSema and stopTheWorldWithSema.
// Holding worldsema causes any other goroutines invoking
// stopTheWorld to block.
// stopTheWorldWithSema STW的核心实现
func stopTheWorldWithSema() {
	_g_ := getg()

	// If we hold a lock, then we won't be able to stop another M
	// that is blocked trying to acquire the lock.
	if _g_.m.locks > 0 {
		throw("stopTheWorld: holding locks")
	}

	lock(&sched.lock)
	sched.stopwait = gomaxprocs
	// 设置gc等待标记, 调度时看见此标记会进入等待
	atomic.Store(&sched.gcwaiting, 1)
	// 抢占所有运行中的G
	preemptall()
	// stop current P
	_g_.m.p.ptr().status = _Pgcstop // Pgcstop is only diagnostic.
	sched.stopwait--

	// 停止所有的P
	// try to retake all P's in Psyscall status
	for _, p := range allp {
		s := p.status
		if s == _Psyscall && atomic.Cas(&p.status, s, _Pgcstop) {
			if trace.enabled {
				traceGoSysBlock(p)
				traceProcStop(p)
			}
			p.syscalltick++
			sched.stopwait--
		}
	}
	// stop idle P's
	for {
		p := pidleget()
		if p == nil {
			break
		}
		p.status = _Pgcstop
		sched.stopwait--
	}
	wait := sched.stopwait > 0
	unlock(&sched.lock)

	// wait for remaining P's to stop voluntarily
	if wait {
		for {
			// wait for 100us, then try to re-preempt in case of any races
			if notetsleep(&sched.stopnote, 100*1000) {
				noteclear(&sched.stopnote)
				break
			}
			preemptall()
		}
	}

	// sanity checks
	bad := ""
	if sched.stopwait != 0 {
		bad = "stopTheWorld: not stopped (stopwait != 0)"
	} else {
		for _, p := range allp {
			if p.status != _Pgcstop {
				bad = "stopTheWorld: not stopped (status != _Pgcstop)"
			}
		}
	}
	if atomic.Load(&freezing) != 0 {
		// Some other thread is panicking. This can cause the
		// sanity checks above to fail if the panic happens in
		// the signal handler on a stopped thread. Either way,
		// we should halt this thread.
		lock(&deadlock)
		lock(&deadlock)
	}
	if bad != "" {
		throw(bad)
	}
}

func mhelpgc() {
	_g_ := getg()
	_g_.m.helpgc = -1
}

// 启动世界的核心实现，返回开始的时间
func startTheWorldWithSema(emitTraceEvent bool) int64 {
	_g_ := getg()

	_g_.m.locks++ // disable preemption because it can be holding p in a local var
	if netpollinited() {
		gp := netpoll(false) // non-blocking
		injectglist(gp)
	}
	add := needaddgcproc()
	lock(&sched.lock)

	procs := gomaxprocs
	if newprocs != 0 {
		procs = newprocs
		newprocs = 0
	}
	p1 := procresize(procs)
	sched.gcwaiting = 0
	if sched.sysmonwait != 0 {
		sched.sysmonwait = 0
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)

	for p1 != nil {
		p := p1
		p1 = p1.link.ptr()
		if p.m != 0 {
			mp := p.m.ptr()
			p.m = 0
			if mp.nextp != 0 {
				throw("startTheWorld: inconsistent mp->nextp")
			}
			mp.nextp.set(p)
			notewakeup(&mp.park)
		} else {
			// Start M to run P.  Do not start another M below.
			newm(nil, p)
			add = false
		}
	}

	// Capture start-the-world time before doing clean-up tasks.
	startTime := nanotime()
	if emitTraceEvent {
		traceGCSTWDone()
	}

	// Wakeup an additional proc in case we have excessive runnable goroutines
	// in local queues or in the global queue. If we don't, the proc will park itself.
	// If we have lots of excessive work, resetspinning will unpark additional procs as necessary.
	if atomic.Load(&sched.npidle) != 0 && atomic.Load(&sched.nmspinning) == 0 {
		wakep()
	}

	if add {
		// If GC could have used another helper proc, start one now,
		// in the hope that it will be available next time.
		// It would have been even better to start it before the collection,
		// but doing so requires allocating memory, so it's tricky to
		// coordinate. This lazy approach works out in practice:
		// we don't mind if the first couple gc rounds don't have quite
		// the maximum number of procs.
		newm(mhelpgc, nil)
	}
	_g_.m.locks--
	if _g_.m.locks == 0 && _g_.preempt { // restore the preemption request in case we've cleared it in newstack
		_g_.stackguard0 = stackPreempt
	}

	return startTime
}

// Called to start an M.
//
// This must not split the stack because we may not even have stack
// bounds set up yet.
//
// May run during STW (because it doesn't have a P yet), so write
// barriers are not allowed.
//
//go:nosplit
//go:nowritebarrierrec
// 启动一个M，并且陷入调度中，这是一切调度的开始，如果要看调度系统，先看schedinit，然后就看这
// 启动一个M，除了mstart，还有startm这个函数，会按需分配新的m
// 调度的机制用一句话描述：
// runtime准备好G,P,M，然后M绑定P，M从各种队列中获取G，切换到G的执行栈上并执行G上的任务函数，调用goexit做清理工作并回到M，如此反复。
/*
                            +-------------------- sysmon ---------------//------+
                            |                                                   |
                            |                                                   |
               +---+      +---+-------+                   +--------+          +---+---+
go func() ---> | G | ---> | P | local | <=== balance ===> | global | <--//--- | P | M |
               +---+      +---+-------+                   +--------+          +---+---+
                            |                                 |                 |
                            |      +---+                      |                 |
                            +----> | M | <--- findrunnable ---+--- steal <--//--+
                                   +---+
                                     |
                                   mstart
                                     |
              +--- execute <----- schedule
              |                      |
              |                      |
              +--> G.fn --> goexit --+


              1. go func() 语气创建G。
              2. 将G放入P的本地队列（或者平衡到全局全局队列）。
              3. 唤醒或新建M来执行任务。
              4. 进入调度循环
              5. 尽力获取可执行的G，并执行
              6. 清理现场并且重新进入调度循环
*/
func mstart() {
	// 这里获取的g是g0，在系统堆栈
	_g_ := getg()

	// 检查栈边界
	osStack := _g_.stack.lo == 0
	if osStack {
		// Initialize stack bounds from system stack.
		// Cgo may have left stack size in stack.hi.
		size := _g_.stack.hi
		if size == 0 {
			size = 8192 * sys.StackGuardMultiplier // 8MB大小的栈
		}
		_g_.stack.hi = uintptr(noescape(unsafe.Pointer(&size)))
		_g_.stack.lo = _g_.stack.hi - size + 1024
	}
	// Initialize stack guards so that we can start calling
	// both Go and C functions with stack growth prologues.
	// 初始化堆栈保护，以便我们可以开始使用堆栈增长序列调用Go和C函数。
	_g_.stackguard0 = _g_.stack.lo + _StackGuard
	_g_.stackguard1 = _g_.stackguard0

	mstart1(0)

	// Exit this thread.
	if GOOS == "windows" || GOOS == "solaris" || GOOS == "plan9" {
		// Window, Solaris and Plan 9 always system-allocate
		// the stack, but put it in _g_.stack before mstart,
		// so the logic above hasn't set osStack yet.
		osStack = true
	}
	mexit(osStack)
}

// dummy一直为0，给getcallersp当参数
func mstart1(dummy int32) {
	_g_ := getg()

	// 确保g是系统栈上的g0
	// 调度器只在g0上执行
	if _g_ != _g_.m.g0 {
		throw("bad runtime·mstart")
	}

	// Record the caller for use as the top of stack in mcall and
	// for terminating the thread.
	// We're never coming back to mstart1 after we call schedule,
	// so other calls can reuse the current frame.
	// 记录 mstart1 函数结束后的地址pc和mstart1 函数参数到当前g的运行现场
	save(getcallerpc(), getcallersp(unsafe.Pointer(&dummy)))
	asminit()
	// 初始化m，主要是设置线程的备用信号堆栈和信号掩码
	minit()

	// Install signal handlers; after minit so that minit can
	// prepare the thread to be able to handle the signals.
	// 如果当前g的m是初始m0，执行mstartm0()
	if _g_.m == &m0 {
		// 对于初始m，需要一些特殊处理，主要是设置系统信号量的处理函数
		mstartm0()
	}

	// 如果有m的起始任务函数，则执行，比如 sysmon 函数
	// 对于m0来说，是没有 mstartfn 的
	if fn := _g_.m.mstartfn; fn != nil {
		fn()
	}

	if _g_.m.helpgc != 0 {
		_g_.m.helpgc = 0
		stopm()
	} else if _g_.m != &m0 { // 如果不是m0，需要绑定p
		// 绑定p
		acquirep(_g_.m.nextp.ptr())
		_g_.m.nextp = 0
	}

	// 进入调度，而且不会在返回
	schedule()
}

// mstartm0 implements part of mstart1 that only runs on the m0.
//
// Write barriers are allowed here because we know the GC can't be
// running yet, so they'll be no-ops.
//
//go:yeswritebarrierrec
func mstartm0() {
	// Create an extra M for callbacks on threads not created by Go.
	// 判断是cgo 且 还没有额外的M，那么新建一个额外的M
	if iscgo && !cgoHasExtraM {
		cgoHasExtraM = true
		newextram()
	}
	// 系统信号量的初始化
	initsig(false)
}

// mexit tears down and exits the current thread.
//
// Don't call this directly to exit the thread, since it must run at
// the top of the thread stack. Instead, use gogo(&_g_.m.g0.sched) to
// unwind the stack to the point that exits the thread.
//
// It is entered with m.p != nil, so write barriers are allowed. It
// will release the P before exiting.
//
//go:yeswritebarrierrec
func mexit(osStack bool) {
	g := getg()
	m := g.m

	if m == &m0 {
		// This is the main thread. Just wedge it.
		//
		// On Linux, exiting the main thread puts the process
		// into a non-waitable zombie state. On Plan 9,
		// exiting the main thread unblocks wait even though
		// other threads are still running. On Solaris we can
		// neither exitThread nor return from mstart. Other
		// bad things probably happen on other platforms.
		//
		// We could try to clean up this M more before wedging
		// it, but that complicates signal handling.
		handoffp(releasep())
		lock(&sched.lock)
		sched.nmfreed++
		checkdead()
		unlock(&sched.lock)
		notesleep(&m.park)
		throw("locked m0 woke up")
	}

	sigblock()
	unminit()

	// Free the gsignal stack.
	if m.gsignal != nil {
		stackfree(m.gsignal.stack)
	}

	// Remove m from allm.
	lock(&sched.lock)
	for pprev := &allm; *pprev != nil; pprev = &(*pprev).alllink {
		if *pprev == m {
			*pprev = m.alllink
			goto found
		}
	}
	throw("m not found in allm")
found:
	if !osStack {
		// Delay reaping m until it's done with the stack.
		//
		// If this is using an OS stack, the OS will free it
		// so there's no need for reaping.
		atomic.Store(&m.freeWait, 1)
		// Put m on the free list, though it will not be reaped until
		// freeWait is 0. Note that the free list must not be linked
		// through alllink because some functions walk allm without
		// locking, so may be using alllink.
		m.freelink = sched.freem
		sched.freem = m
	}
	unlock(&sched.lock)

	// Release the P.
	handoffp(releasep())
	// After this point we must not have write barriers.

	// Invoke the deadlock detector. This must happen after
	// handoffp because it may have started a new M to take our
	// P's work.
	lock(&sched.lock)
	sched.nmfreed++
	checkdead()
	unlock(&sched.lock)

	if osStack {
		// Return from mstart and let the system thread
		// library free the g0 stack and terminate the thread.
		return
	}

	// mstart is the thread's entry point, so there's nothing to
	// return to. Exit the thread directly. exitThread will clear
	// m.freeWait when it's done with the stack and the m can be
	// reaped.
	exitThread(&m.freeWait)
}

// forEachP calls fn(p) for every P p when p reaches a GC safe point.
// If a P is currently executing code, this will bring the P to a GC
// safe point and execute fn on that P. If the P is not executing code
// (it is idle or in a syscall), this will call fn(p) directly while
// preventing the P from exiting its state. This does not ensure that
// fn will run on every CPU executing Go code, but it acts as a global
// memory barrier. GC uses this as a "ragged barrier."
//
// The caller must hold worldsema.
//
//go:systemstack
func forEachP(fn func(*p)) {
	mp := acquirem()
	_p_ := getg().m.p.ptr()

	lock(&sched.lock)
	if sched.safePointWait != 0 {
		throw("forEachP: sched.safePointWait != 0")
	}
	sched.safePointWait = gomaxprocs - 1
	sched.safePointFn = fn

	// Ask all Ps to run the safe point function.
	for _, p := range allp {
		if p != _p_ {
			atomic.Store(&p.runSafePointFn, 1)
		}
	}
	preemptall()

	// Any P entering _Pidle or _Psyscall from now on will observe
	// p.runSafePointFn == 1 and will call runSafePointFn when
	// changing its status to _Pidle/_Psyscall.

	// Run safe point function for all idle Ps. sched.pidle will
	// not change because we hold sched.lock.
	for p := sched.pidle.ptr(); p != nil; p = p.link.ptr() {
		if atomic.Cas(&p.runSafePointFn, 1, 0) {
			fn(p)
			sched.safePointWait--
		}
	}

	wait := sched.safePointWait > 0
	unlock(&sched.lock)

	// Run fn for the current P.
	fn(_p_)

	// Force Ps currently in _Psyscall into _Pidle and hand them
	// off to induce safe point function execution.
	for _, p := range allp {
		s := p.status
		if s == _Psyscall && p.runSafePointFn == 1 && atomic.Cas(&p.status, s, _Pidle) {
			if trace.enabled {
				traceGoSysBlock(p)
				traceProcStop(p)
			}
			p.syscalltick++
			handoffp(p)
		}
	}

	// Wait for remaining Ps to run fn.
	if wait {
		for {
			// Wait for 100us, then try to re-preempt in
			// case of any races.
			//
			// Requires system stack.
			if notetsleep(&sched.safePointNote, 100*1000) {
				noteclear(&sched.safePointNote)
				break
			}
			preemptall()
		}
	}
	if sched.safePointWait != 0 {
		throw("forEachP: not done")
	}
	for _, p := range allp {
		if p.runSafePointFn != 0 {
			throw("forEachP: P did not run fn")
		}
	}

	lock(&sched.lock)
	sched.safePointFn = nil
	unlock(&sched.lock)
	releasem(mp)
}

// runSafePointFn runs the safe point function, if any, for this P.
// This should be called like
//
//     if getg().m.p.runSafePointFn != 0 {
//         runSafePointFn()
//     }
//
// runSafePointFn must be checked on any transition in to _Pidle or
// _Psyscall to avoid a race where forEachP sees that the P is running
// just before the P goes into _Pidle/_Psyscall and neither forEachP
// nor the P run the safe-point function.
func runSafePointFn() {
	p := getg().m.p.ptr()
	// Resolve the race between forEachP running the safe-point
	// function on this P's behalf and this P running the
	// safe-point function directly.
	if !atomic.Cas(&p.runSafePointFn, 1, 0) {
		return
	}
	sched.safePointFn(p)
	lock(&sched.lock)
	sched.safePointWait--
	if sched.safePointWait == 0 {
		notewakeup(&sched.safePointNote)
	}
	unlock(&sched.lock)
}

// When running with cgo, we call _cgo_thread_start
// to start threads for us so that we can play nicely with
// foreign code.
var cgoThreadStart unsafe.Pointer

type cgothreadstart struct {
	g   guintptr
	tls *uint64
	fn  unsafe.Pointer
}

// Allocate a new m unassociated with any thread.
// Can use p for allocation context if needed.
// fn is recorded as the new m's m.mstartfn.
//
// This function is allowed to have write barriers even if the caller
// isn't because it borrows _p_.
//
//go:yeswritebarrierrec
// 分配一个m，且不关联任何一个os thread
func allocm(_p_ *p, fn func()) *m {
	_g_ := getg()
	_g_.m.locks++ // disable GC because it can be called from sysmon
	if _g_.m.p == 0 {
		acquirep(_p_) // temporarily borrow p for mallocs in this function
	}

	// Release the free M list. We need to do this somewhere and
	// this may free up a stack we can use.
	if sched.freem != nil {
		lock(&sched.lock)
		var newList *m
		for freem := sched.freem; freem != nil; {
			if freem.freeWait != 0 {
				next := freem.freelink
				freem.freelink = newList
				newList = freem
				freem = next
				continue
			}
			stackfree(freem.g0.stack)
			freem = freem.freelink
		}
		sched.freem = newList
		unlock(&sched.lock)
	}

	mp := new(m)
	mp.mstartfn = fn
	mcommoninit(mp)

	// In case of cgo or Solaris, pthread_create will make us a stack.
	// Windows and Plan 9 will layout sched stack on OS stack.
	// 给g0分配一定的堆栈
	if iscgo || GOOS == "solaris" || GOOS == "windows" || GOOS == "plan9" {
		mp.g0 = malg(-1)
	} else {
		mp.g0 = malg(8192 * sys.StackGuardMultiplier)
	}
	mp.g0.m = mp

	if _p_ == _g_.m.p.ptr() {
		releasep()
	}
	_g_.m.locks--
	if _g_.m.locks == 0 && _g_.preempt { // restore the preemption request in case we've cleared it in newstack
		_g_.stackguard0 = stackPreempt
	}

	return mp
}

// needm is called when a cgo callback happens on a
// thread without an m (a thread not created by Go).
// In this case, needm is expected to find an m to use
// and return with m, g initialized correctly.
// Since m and g are not set now (likely nil, but see below)
// needm is limited in what routines it can call. In particular
// it can only call nosplit functions (textflag 7) and cannot
// do any scheduling that requires an m.
//
// In order to avoid needing heavy lifting here, we adopt
// the following strategy: there is a stack of available m's
// that can be stolen. Using compare-and-swap
// to pop from the stack has ABA races, so we simulate
// a lock by doing an exchange (via casp) to steal the stack
// head and replace the top pointer with MLOCKED (1).
// This serves as a simple spin lock that we can use even
// without an m. The thread that locks the stack in this way
// unlocks the stack by storing a valid stack head pointer.
//
// In order to make sure that there is always an m structure
// available to be stolen, we maintain the invariant that there
// is always one more than needed. At the beginning of the
// program (if cgo is in use) the list is seeded with a single m.
// If needm finds that it has taken the last m off the list, its job
// is - once it has installed its own m so that it can do things like
// allocate memory - to create a spare m and put it on the list.
//
// Each of these extra m's also has a g0 and a curg that are
// pressed into service as the scheduling stack and current
// goroutine for the duration of the cgo callback.
//
// When the callback is done with the m, it calls dropm to
// put the m back on the list.
//go:nosplit
func needm(x byte) {
	if iscgo && !cgoHasExtraM {
		// Can happen if C/C++ code calls Go from a global ctor.
		// Can not throw, because scheduler is not initialized yet.
		write(2, unsafe.Pointer(&earlycgocallback[0]), int32(len(earlycgocallback)))
		exit(1)
	}

	// Lock extra list, take head, unlock popped list.
	// nilokay=false is safe here because of the invariant above,
	// that the extra list always contains or will soon contain
	// at least one m.
	mp := lockextra(false)

	// Set needextram when we've just emptied the list,
	// so that the eventual call into cgocallbackg will
	// allocate a new m for the extra list. We delay the
	// allocation until then so that it can be done
	// after exitsyscall makes sure it is okay to be
	// running at all (that is, there's no garbage collection
	// running right now).
	mp.needextram = mp.schedlink == 0
	extraMCount--
	unlockextra(mp.schedlink.ptr())

	// Save and block signals before installing g.
	// Once g is installed, any incoming signals will try to execute,
	// but we won't have the sigaltstack settings and other data
	// set up appropriately until the end of minit, which will
	// unblock the signals. This is the same dance as when
	// starting a new m to run Go code via newosproc.
	msigsave(mp)
	sigblock()

	// Install g (= m->g0) and set the stack bounds
	// to match the current stack. We don't actually know
	// how big the stack is, like we don't know how big any
	// scheduling stack is, but we assume there's at least 32 kB,
	// which is more than enough for us.
	setg(mp.g0)
	_g_ := getg()
	_g_.stack.hi = uintptr(noescape(unsafe.Pointer(&x))) + 1024
	_g_.stack.lo = uintptr(noescape(unsafe.Pointer(&x))) - 32*1024
	_g_.stackguard0 = _g_.stack.lo + _StackGuard

	// Initialize this thread to use the m.
	asminit()
	minit()

	// mp.curg is now a real goroutine.
	casgstatus(mp.curg, _Gdead, _Gsyscall)
	atomic.Xadd(&sched.ngsys, -1)
}

var earlycgocallback = []byte("fatal error: cgo callback before cgo call\n")

// newextram allocates m's and puts them on the extra list.
// It is called with a working local m, so that it can do things
// like call schedlock and allocate.
func newextram() {
	c := atomic.Xchg(&extraMWaiters, 0)
	if c > 0 {
		for i := uint32(0); i < c; i++ {
			oneNewExtraM()
		}
	} else {
		// Make sure there is at least one extra M.
		mp := lockextra(true)
		unlockextra(mp)
		if mp == nil {
			oneNewExtraM()
		}
	}
}

// oneNewExtraM allocates an m and puts it on the extra list.
func oneNewExtraM() {
	// Create extra goroutine locked to extra m.
	// The goroutine is the context in which the cgo callback will run.
	// The sched.pc will never be returned to, but setting it to
	// goexit makes clear to the traceback routines where
	// the goroutine stack ends.
	mp := allocm(nil, nil)
	gp := malg(4096)
	gp.sched.pc = funcPC(goexit) + sys.PCQuantum
	gp.sched.sp = gp.stack.hi
	gp.sched.sp -= 4 * sys.RegSize // extra space in case of reads slightly beyond frame
	gp.sched.lr = 0
	gp.sched.g = guintptr(unsafe.Pointer(gp))
	gp.syscallpc = gp.sched.pc
	gp.syscallsp = gp.sched.sp
	gp.stktopsp = gp.sched.sp
	gp.gcscanvalid = true
	gp.gcscandone = true
	// malg returns status as _Gidle. Change to _Gdead before
	// adding to allg where GC can see it. We use _Gdead to hide
	// this from tracebacks and stack scans since it isn't a
	// "real" goroutine until needm grabs it.
	casgstatus(gp, _Gidle, _Gdead)
	gp.m = mp
	mp.curg = gp
	mp.lockedInt++
	mp.lockedg.set(gp)
	gp.lockedm.set(mp)
	gp.goid = int64(atomic.Xadd64(&sched.goidgen, 1))
	if raceenabled {
		gp.racectx = racegostart(funcPC(newextram) + sys.PCQuantum)
	}
	// put on allg for garbage collector
	allgadd(gp)

	// gp is now on the allg list, but we don't want it to be
	// counted by gcount. It would be more "proper" to increment
	// sched.ngfree, but that requires locking. Incrementing ngsys
	// has the same effect.
	atomic.Xadd(&sched.ngsys, +1)

	// Add m to the extra list.
	mnext := lockextra(true)
	mp.schedlink.set(mnext)
	extraMCount++
	unlockextra(mp)
}

// dropm is called when a cgo callback has called needm but is now
// done with the callback and returning back into the non-Go thread.
// It puts the current m back onto the extra list.
//
// The main expense here is the call to signalstack to release the
// m's signal stack, and then the call to needm on the next callback
// from this thread. It is tempting to try to save the m for next time,
// which would eliminate both these costs, but there might not be
// a next time: the current thread (which Go does not control) might exit.
// If we saved the m for that thread, there would be an m leak each time
// such a thread exited. Instead, we acquire and release an m on each
// call. These should typically not be scheduling operations, just a few
// atomics, so the cost should be small.
//
// TODO(rsc): An alternative would be to allocate a dummy pthread per-thread
// variable using pthread_key_create. Unlike the pthread keys we already use
// on OS X, this dummy key would never be read by Go code. It would exist
// only so that we could register at thread-exit-time destructor.
// That destructor would put the m back onto the extra list.
// This is purely a performance optimization. The current version,
// in which dropm happens on each cgo call, is still correct too.
// We may have to keep the current version on systems with cgo
// but without pthreads, like Windows.
func dropm() {
	// Clear m and g, and return m to the extra list.
	// After the call to setg we can only call nosplit functions
	// with no pointer manipulation.
	mp := getg().m

	// Return mp.curg to dead state.
	casgstatus(mp.curg, _Gsyscall, _Gdead)
	atomic.Xadd(&sched.ngsys, +1)

	// Block signals before unminit.
	// Unminit unregisters the signal handling stack (but needs g on some systems).
	// Setg(nil) clears g, which is the signal handler's cue not to run Go handlers.
	// It's important not to try to handle a signal between those two steps.
	sigmask := mp.sigmask
	sigblock()
	unminit()

	mnext := lockextra(true)
	extraMCount++
	mp.schedlink.set(mnext)

	setg(nil)

	// Commit the release of mp.
	unlockextra(mp)

	msigrestore(sigmask)
}

// A helper function for EnsureDropM.
func getm() uintptr {
	return uintptr(unsafe.Pointer(getg().m))
}

var extram uintptr
var extraMCount uint32 // Protected by lockextra
var extraMWaiters uint32

// lockextra locks the extra list and returns the list head.
// The caller must unlock the list by storing a new list head
// to extram. If nilokay is true, then lockextra will
// return a nil list head if that's what it finds. If nilokay is false,
// lockextra will keep waiting until the list head is no longer nil.
//go:nosplit
func lockextra(nilokay bool) *m {
	const locked = 1

	incr := false
	for {
		old := atomic.Loaduintptr(&extram)
		if old == locked {
			yield := osyield
			yield()
			continue
		}
		if old == 0 && !nilokay {
			if !incr {
				// Add 1 to the number of threads
				// waiting for an M.
				// This is cleared by newextram.
				atomic.Xadd(&extraMWaiters, 1)
				incr = true
			}
			usleep(1)
			continue
		}
		if atomic.Casuintptr(&extram, old, locked) {
			return (*m)(unsafe.Pointer(old))
		}
		yield := osyield
		yield()
		continue
	}
}

//go:nosplit
func unlockextra(mp *m) {
	atomic.Storeuintptr(&extram, uintptr(unsafe.Pointer(mp)))
}

// execLock serializes exec and clone to avoid bugs or unspecified behaviour
// around exec'ing while creating/destroying threads.  See issue #19546.
var execLock rwmutex

// newmHandoff contains a list of m structures that need new OS threads.
// This is used by newm in situations where newm itself can't safely
// start an OS thread.
// newmHandoff 包含需要新OS线程的m个结构的列表。在newm本身无法安全启动OS线程的情况下，newm会使用它。
var newmHandoff struct {
	lock mutex

	// newm points to a list of M structures that need new OS
	// threads. The list is linked through m.schedlink.
	newm muintptr

	// waiting indicates that wake needs to be notified when an m
	// is put on the list.
	waiting bool
	wake    note

	// haveTemplateThread indicates that the templateThread has
	// been started. This is not protected by lock. Use cas to set
	// to 1.
	haveTemplateThread uint32
}

// Create a new m. It will start off with a call to fn, or else the scheduler.
// fn needs to be static and not a heap allocated closure.
// May run with m.p==nil, so write barriers are not allowed.
//go:nowritebarrierrec
// 创建一个新的m，它将从fn或者调度程序开始
// fn需要是静态的，而不是堆分配的闭包。
func newm(fn func(), _p_ *p) {
	// 根据fn和p和绑定一个m对象
	mp := allocm(_p_, fn)
	// 设置当前m的下一个p为_p_
	mp.nextp.set(_p_)
	mp.sigmask = initSigmask
	if gp := getg(); gp != nil && gp.m != nil && (gp.m.lockedExt != 0 || gp.m.incgo) && GOOS != "plan9" {
		// We're on a locked M or a thread that may have been
		// started by C. The kernel state of this thread may
		// be strange (the user may have locked it for that
		// purpose). We don't want to clone that into another
		// thread. Instead, ask a known-good thread to create
		// the thread for us.
		//
		// This is disabled on Plan 9. See golang.org/issue/22227.
		//
		// TODO: This may be unnecessary on Windows, which
		// doesn't model thread creation off fork.
		// 我们处于锁定的M或可能由C启动的线程。此线程的内核状态可能很奇怪（用户可能已将其锁定为此目的）。
		// 我们不想将其克隆到另一个线程中。相反，请求一个已知良好的线程为我们创建线程。
		lock(&newmHandoff.lock)
		if newmHandoff.haveTemplateThread == 0 {
			throw("on a locked thread with no template thread")
		}
		mp.schedlink = newmHandoff.newm
		newmHandoff.newm.set(mp)
		if newmHandoff.waiting {
			newmHandoff.waiting = false
			notewakeup(&newmHandoff.wake)
		}
		unlock(&newmHandoff.lock)
		return
	}
	// 真正的分配os thread
	newm1(mp)
}

func newm1(mp *m) {
	// 对cgo的处理
	if iscgo {
		var ts cgothreadstart
		if _cgo_thread_start == nil {
			throw("_cgo_thread_start missing")
		}
		ts.g.set(mp.g0)
		ts.tls = (*uint64)(unsafe.Pointer(&mp.tls[0]))
		ts.fn = unsafe.Pointer(funcPC(mstart))
		if msanenabled {
			msanwrite(unsafe.Pointer(&ts), unsafe.Sizeof(ts))
		}
		execLock.rlock() // Prevent process clone.
		asmcgocall(_cgo_thread_start, unsafe.Pointer(&ts))
		execLock.runlock()
		return
	}
	execLock.rlock() // Prevent process clone.
	// 创建一个系统线程，并且传入该 mp 绑定的 g0 的栈顶指针
	// 让系统线程执行 mstart 函数，后面的逻辑都在 mstart 函数中
	newosproc(mp, unsafe.Pointer(mp.g0.stack.hi))
	execLock.runlock()
}

// startTemplateThread starts the template thread if it is not already
// running.
//
// The calling thread must itself be in a known-good state.
// startTemplateThread 启动模板线程（如果尚未运行）。
// 调用线程本身必须处于已知良好状态。
func startTemplateThread() {
	if !atomic.Cas(&newmHandoff.haveTemplateThread, 0, 1) {
		return
	}
	newm(templateThread, nil)
}

// tmeplateThread is a thread in a known-good state that exists solely
// to start new threads in known-good states when the calling thread
// may not be a a good state.
//
// Many programs never need this, so templateThread is started lazily
// when we first enter a state that might lead to running on a thread
// in an unknown state.
//
// templateThread runs on an M without a P, so it must not have write
// barriers.
//
//go:nowritebarrierrec
// tmeplateThread 是处于已知良好状态的线程，仅当调用线程可能不是良好状态时，该线程仅用于以已知良好状态启动新线程。
// 许多程序从不需要这个，所以当我们第一次进入可能导致在未知状态的线程上运行的状态时，templateThread 会懒得开始。
// templateThread在没有P的M上运行，因此它必须没有写入障碍。
func templateThread() {
	lock(&sched.lock)
	sched.nmsys++
	checkdead()
	unlock(&sched.lock)

	for {
		lock(&newmHandoff.lock)
		for newmHandoff.newm != 0 {
			newm := newmHandoff.newm.ptr()
			newmHandoff.newm = 0
			unlock(&newmHandoff.lock)
			for newm != nil {
				next := newm.schedlink.ptr()
				newm.schedlink = 0
				newm1(newm)
				newm = next
			}
			lock(&newmHandoff.lock)
		}
		newmHandoff.waiting = true
		noteclear(&newmHandoff.wake)
		unlock(&newmHandoff.lock)
		notesleep(&newmHandoff.wake)
	}
}

// Stops execution of the current m until new work is available.
// Returns with acquired P.
// 停止M，使其休眠，并把该m放进空闲m的链表中。一般会调用 startm 来唤醒休眠的m
// 调用notesleep使M进入休眠
// 线程可以处于三种状态: 等待中(Waiting)、待执行(Runnable)或执行中(Executing)。
func stopm() {
	_g_ := getg()

	if _g_.m.locks != 0 {
		throw("stopm holding locks")
	}
	if _g_.m.p != 0 {
		throw("stopm holding p")
	}
	if _g_.m.spinning {
		throw("stopm spinning")
	}

retry:
	lock(&sched.lock)
	mput(_g_.m)
	unlock(&sched.lock)
	// 在lock_futex.go 中
	notesleep(&_g_.m.park)
	noteclear(&_g_.m.park)
	if _g_.m.helpgc != 0 {
		// helpgc() set _g_.m.p and _g_.m.mcache, so we have a P.
		gchelper()
		// Undo the effects of helpgc().
		_g_.m.helpgc = 0
		_g_.m.mcache = nil
		_g_.m.p = 0
		goto retry
	}
	acquirep(_g_.m.nextp.ptr())
	_g_.m.nextp = 0
}

// 设置当前g的m为自旋状态
func mspinning() {
	// startm's caller incremented nmspinning. Set the new M's spinning.
	getg().m.spinning = true
}

// Schedules some M to run the p (creates an M if necessary).
// If p==nil, tries to get an idle P, if no idle P's does nothing.
// May run with m.p==nil, so write barriers are not allowed.
// If spinning is set, the caller has incremented nmspinning and startm will
// either decrement nmspinning or set m.spinning in the newly started M.
//go:nowritebarrierrec
// startm是启动一个M，先尝试获取一个空闲P，如果获取不到则返回
// 获取到P后，在尝试获取M，如果获取不到就新建一个M
func startm(_p_ *p, spinning bool) {
	lock(&sched.lock)
	// 如果P为nil，则尝试获取一个空闲P
	if _p_ == nil {
		_p_ = pidleget()
		if _p_ == nil {
			unlock(&sched.lock)
			if spinning {
				// The caller incremented nmspinning, but there are no idle Ps,
				// so it's okay to just undo the increment and give up.
				if int32(atomic.Xadd(&sched.nmspinning, -1)) < 0 {
					throw("startm: negative nmspinning")
				}
			}
			return
		}
	}
	// 获取一个空闲的M
	mp := mget()
	unlock(&sched.lock)
	if mp == nil {
		var fn func()
		if spinning {
			// The caller incremented nmspinning, so set m.spinning in the new M.
			fn = mspinning
		}
		// 如果获取不到，则新建一个，新建完成后就立即返回
		newm(fn, _p_)
		return
	}

	// 到这里表示获取到了一个空闲M
	if mp.spinning { // 从midle中获取的mp，不应该是spinning状态，获取的都是经过stopm的，stopm之前都会推出spinning
		throw("startm: m is spinning")
	}
	if mp.nextp != 0 { // 这个位置是要留给参数_p_的，stopm中如果被唤醒，则关联nextp和m
		throw("startm: m has p")
	}
	if spinning && !runqempty(_p_) { // spinning状态的M是在本地和全局都获取不到工作的情况，不能与spinning语义矛盾
		throw("startm: p has runnable gs")
	}
	// The caller incremented nmspinning, so set m.spinning in the new M.
	mp.spinning = spinning //标记该M是否在自旋
	mp.nextp.set(_p_)      // 暂存P
	notewakeup(&mp.park)   // 唤醒M
}

// Hands off P from syscall or locked M.
// Always runs without a P, so write barriers are not allowed.
//go:nowritebarrierrec
// p的切换，系统调用或者绑定M时使用
func handoffp(_p_ *p) {
	// handoffp must start an M in any situation where
	// findrunnable would return a G to run on _p_.

	// if it has local work, start it straight away
	if !runqempty(_p_) || sched.runqsize != 0 {
		startm(_p_, false)
		return
	}
	// if it has GC work, start it straight away
	if gcBlackenEnabled != 0 && gcMarkWorkAvailable(_p_) {
		startm(_p_, false)
		return
	}
	// no local work, check that there are no spinning/idle M's,
	// otherwise our help is not required
	if atomic.Load(&sched.nmspinning)+atomic.Load(&sched.npidle) == 0 && atomic.Cas(&sched.nmspinning, 0, 1) { // TODO: fast atomic
		startm(_p_, true)
		return
	}
	lock(&sched.lock)
	if sched.gcwaiting != 0 {
		_p_.status = _Pgcstop
		sched.stopwait--
		if sched.stopwait == 0 {
			notewakeup(&sched.stopnote)
		}
		unlock(&sched.lock)
		return
	}
	if _p_.runSafePointFn != 0 && atomic.Cas(&_p_.runSafePointFn, 1, 0) {
		sched.safePointFn(_p_)
		sched.safePointWait--
		if sched.safePointWait == 0 {
			notewakeup(&sched.safePointNote)
		}
	}
	if sched.runqsize != 0 {
		unlock(&sched.lock)
		startm(_p_, false)
		return
	}
	// If this is the last running P and nobody is polling network,
	// need to wakeup another M to poll network.
	if sched.npidle == uint32(gomaxprocs-1) && atomic.Load64(&sched.lastpoll) != 0 {
		unlock(&sched.lock)
		startm(_p_, false)
		return
	}
	pidleput(_p_)
	unlock(&sched.lock)
}

// Tries to add one more P to execute G's.
// Called when a G is made runnable (newproc, ready).
// 尝试获取一个M来运行可运行的G
func wakep() {
	// be conservative about spinning threads
	// 如果有其他的M处于自旋状态，那么就不管了，直接返回
	// 因为自旋的M会拼命找G来运行的，就不新找一个M（劳动者）来运行了。
	if !atomic.Cas(&sched.nmspinning, 0, 1) {
		return
	}
	print("startm\n")
	startm(nil, true)
}

// Stops execution of the current m that is locked to a g until the g is runnable again.
// Returns with acquired P.
// 释放P，并且让M进入休眠，等待被唤醒
func stoplockedm() {
	_g_ := getg()

	if _g_.m.lockedg == 0 || _g_.m.lockedg.ptr().lockedm.ptr() != _g_.m {
		throw("stoplockedm: inconsistent locking")
	}

	// 如果M绑定了P，那么释放P
	if _g_.m.p != 0 {
		// Schedule another M to run this p.
		_p_ := releasep()
		handoffp(_p_)
	}
	incidlelocked(1)

	// Wait until another thread schedules lockedg again.
	// M休眠直到被唤醒
	notesleep(&_g_.m.park)
	noteclear(&_g_.m.park)
	status := readgstatus(_g_.m.lockedg.ptr())
	if status&^_Gscan != _Grunnable {
		print("runtime:stoplockedm: g is not Grunnable or Gscanrunnable\n")
		dumpgstatus(_g_)
		throw("stoplockedm: not runnable")
	}

	acquirep(_g_.m.nextp.ptr())
	_g_.m.nextp = 0
}

// Schedules the locked m to run the locked gp.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
// 唤醒gp的lockedm，然后让当前的M休眠
func startlockedm(gp *g) {
	_g_ := getg()

	mp := gp.lockedm.ptr()
	if mp == _g_.m {
		throw("startlockedm: locked to me")
	}
	if mp.nextp != 0 {
		throw("startlockedm: m has p")
	}
	// directly handoff current P to the locked m
	incidlelocked(-1)
	// 分配p
	_p_ := releasep()
	mp.nextp.set(_p_)
	// 唤醒gp的lockedm
	notewakeup(&mp.park)
	// 让当前的M休眠
	stopm()
}

// Stops the current m for stopTheWorld.
// Returns when the world is restarted.
// 为了STW，停止当前的M
func gcstopm() {
	_g_ := getg()

	if sched.gcwaiting == 0 {
		throw("gcstopm: not waiting for gc")
	}
	if _g_.m.spinning {
		_g_.m.spinning = false
		// OK to just drop nmspinning here,
		// startTheWorld will unpark threads as necessary.
		if int32(atomic.Xadd(&sched.nmspinning, -1)) < 0 {
			throw("gcstopm: negative nmspinning")
		}
	}
	_p_ := releasep()
	lock(&sched.lock)
	_p_.status = _Pgcstop
	sched.stopwait--
	if sched.stopwait == 0 {
		notewakeup(&sched.stopnote)
	}
	unlock(&sched.lock)
	stopm()
}

// Schedules gp to run on the current M.
// If inheritTime is true, gp inherits the remaining time in the
// current time slice. Otherwise, it starts a new time slice.
// Never returns.
//
// Write barriers are allowed because this is called immediately after
// acquiring a P in several places.
//
//go:yeswritebarrierrec
// 执行goroutine的任务函数
// 如果inheritTime=true，那么当前的G继承剩余的时间片，其实就是不让schedtick累加，
// 这样的话就不会触发每61次从全局队列找G
func execute(gp *g, inheritTime bool) {
	_g_ := getg()

	// 更改gp的状态为_Grunning
	casgstatus(gp, _Grunnable, _Grunning)
	// 置等待时间为0
	gp.waitsince = 0
	// 置可抢占标志为fasle
	gp.preempt = false
	gp.stackguard0 = gp.stack.lo + _StackGuard
	// 如果不是inheritTime，schedtick累加
	if !inheritTime {
		_g_.m.p.ptr().schedtick++
	}
	// 当前的M的G改为gp
	_g_.m.curg = gp
	// gp的M改为当前的M
	gp.m = _g_.m

	// Check whether the profiler needs to be turned on or off.
	hz := sched.profilehz
	// pprof
	if _g_.m.profilehz != hz {
		setThreadCPUProfiler(hz)
	}

	// goroutine trace
	if trace.enabled {
		// GoSysExit has to happen when we have a P, but before GoStart.
		// So we emit it here.
		if gp.syscallsp != 0 && gp.sysblocktraced {
			traceGoSysExit(gp.sysexitticks)
		}
		// 标记gorutine开始的标志
		traceGoStart()
	}

	// gogo由汇编实现， runtime/asm_amd64.s
	// 实现当前的G切换到gp，然后用JMP跳转到G的任务函数
	// 当任务函数执行完后会调用 goexit
	gogo(&gp.sched)
}

// Finds a runnable goroutine to execute.
// Tries to steal from other P's, get g from global queue, poll network.
// 找到一个可以运行的G，不找到就让M休眠，然后等待唤醒，直到找到一个G返回
func findrunnable() (gp *g, inheritTime bool) {
	_g_ := getg()

	// The conditions here and in handoffp must agree: if
	// findrunnable would return a G to run, handoffp must start
	// an M.
	// 此处和handoffp中的条件必须一致：如果findrunnable将返回G运行，则handoffp必须启动M.
top:
	// 当前m绑定的p
	_p_ := _g_.m.p.ptr()
	// 如果gc正等着运行，停止M，也就是STW
	if sched.gcwaiting != 0 {
		gcstopm()
		goto top
	}
	if _p_.runSafePointFn != 0 {
		runSafePointFn()
	}
	// fing是执行finalizer的goroutine
	if fingwait && fingwake {
		if gp := wakefing(); gp != nil {
			ready(gp, 0, true)
		}
	}
	if *cgo_yield != nil {
		asmcgocall(*cgo_yield, nil)
	}

	// local runq
	// 再尝试从本地队列中获取G
	if gp, inheritTime := runqget(_p_); gp != nil {
		return gp, inheritTime
	}

	// global runq
	// 尝试从全局队列中获取G
	if sched.runqsize != 0 {
		lock(&sched.lock)
		gp := globrunqget(_p_, 0)
		unlock(&sched.lock)
		if gp != nil {
			return gp, false
		}
	}

	// Poll network.
	// This netpoll is only an optimization before we resort to stealing.
	// We can safely skip it if there are no waiters or a thread is blocked
	// in netpoll already. If there is any kind of logical race with that
	// blocked thread (e.g. it has already returned from netpoll, but does
	// not set lastpoll yet), this thread will do blocking netpoll below
	// anyway.
	// 从网络IO轮询器中找到就绪的G，把这个G变为可运行的G
	if netpollinited() && atomic.Load(&netpollWaiters) > 0 && atomic.Load64(&sched.lastpoll) != 0 {
		if gp := netpoll(false); gp != nil { // non-blocking
			// netpoll returns list of goroutines linked by schedlink.
			// 如果找到的可运行的网络IO的G列表，则把相关的G插入全局队列
			injectglist(gp.schedlink.ptr())
			// 更改G的状态为_Grunnable，以便下次M能找到这些G来执行
			casgstatus(gp, _Gwaiting, _Grunnable)
			// goroutine trace事件记录-unpark
			if trace.enabled {
				traceGoUnpark(gp, 0)
			}
			return gp, false
		}
	}

	// Steal work from other P's.
	procs := uint32(gomaxprocs)
	// 如果其他P都是空闲的，就不从其他P哪里偷取G了
	if atomic.Load(&sched.npidle) == procs-1 {
		// Either GOMAXPROCS=1 or everybody, except for us, is idle already.
		// New work can appear from returning syscall/cgocall, network or timers.
		// Neither of that submits to local run queues, so no point in stealing.
		goto stop
	}
	// If number of spinning M's >= number of busy P's, block.
	// This is necessary to prevent excessive CPU consumption
	// when GOMAXPROCS>>1 but the program parallelism is low.
	//
	// 如果当前的M没在自旋 且 正在自旋的M个数的2倍>=正在忙的p的个数时，不让该M进入自旋状态
	if !_g_.m.spinning && 2*atomic.Load(&sched.nmspinning) >= procs-atomic.Load(&sched.npidle) {
		goto stop
	}

	// 如果M为非自旋，那么设置为自旋状态
	if !_g_.m.spinning {
		_g_.m.spinning = true
		atomic.Xadd(&sched.nmspinning, 1)
	}
	// 随机选一个P，尝试从这P中偷取一些G
	for i := 0; i < 4; i++ { // 尝试四次
		for enum := stealOrder.start(fastrand()); !enum.done(); enum.next() {
			if sched.gcwaiting != 0 {
				goto top
			}
			stealRunNextG := i > 2 // first look for ready queues with more than 1 g
			// 从allp[enum.position()]偷去一半的G，并返回其中的一个
			if gp := runqsteal(_p_, allp[enum.position()], stealRunNextG); gp != nil {
				return gp, false
			}
		}
	}

stop:

	// We have nothing to do. If we're in the GC mark phase, can
	// safely scan and blacken objects, and have work to do, run
	// idle-time marking rather than give up the P.
	// 当前的M找不到G来运行。如果此时P处于 GC mark 阶段
	// 那么此时可以安全的扫描和黑化对象，和返回 gcBgMarkWorker 来运行
	if gcBlackenEnabled != 0 && _p_.gcBgMarkWorker != 0 && gcMarkWorkAvailable(_p_) {
		// 设置gcMarkWorkerMode 为 gcMarkWorkerIdleMode
		_p_.gcMarkWorkerMode = gcMarkWorkerIdleMode
		// 获取gcBgMarkWorker goroutine
		gp := _p_.gcBgMarkWorker.ptr()
		casgstatus(gp, _Gwaiting, _Grunnable)
		if trace.enabled {
			traceGoUnpark(gp, 0)
		}
		return gp, false
	}

	// Before we drop our P, make a snapshot of the allp slice,
	// which can change underfoot once we no longer block
	// safe-points. We don't need to snapshot the contents because
	// everything up to cap(allp) is immutable.
	allpSnapshot := allp

	// return P and block
	lock(&sched.lock)
	if sched.gcwaiting != 0 || _p_.runSafePointFn != 0 {
		unlock(&sched.lock)
		goto top
	}
	// 再次从全局队列中获取G
	if sched.runqsize != 0 {
		gp := globrunqget(_p_, 0)
		unlock(&sched.lock)
		return gp, false
	}

	// 将当前对M和P解绑
	if releasep() != _p_ {
		throw("findrunnable: wrong p")
	}
	// 将p放入p空闲链表
	pidleput(_p_)
	unlock(&sched.lock)

	// Delicate dance: thread transitions from spinning to non-spinning state,
	// potentially concurrently with submission of new goroutines. We must
	// drop nmspinning first and then check all per-P queues again (with
	// #StoreLoad memory barrier in between). If we do it the other way around,
	// another thread can submit a goroutine after we've checked all run queues
	// but before we drop nmspinning; as the result nobody will unpark a thread
	// to run the goroutine.
	// If we discover new work below, we need to restore m.spinning as a signal
	// for resetspinning to unpark a new worker thread (because there can be more
	// than one starving goroutine). However, if after discovering new work
	// we also observe no idle Ps, it is OK to just park the current thread:
	// the system is fully loaded so no spinning threads are required.
	// Also see "Worker thread parking/unparking" comment at the top of the file.

	// 一些技巧与平衡：线程从旋转状态转换到非旋转状态，可能与提交新的goroutine同时发生。
	// 我们必须首先删除nmspinning，然后再次检查所有per-P队列（中间有#StoreLoad内存屏障）。
	// 如果我们反过来这样做，另一个线程可以在我们检查所有运行队列之后但在我们放弃nmspinning之前提交goroutine;
	// 因此，没有人会取消一个线程来运行goroutine。 如果我们发现下面的新工作，我们需要恢复m.spinning作为重置的信号，
	// 以取消park新的工作线程（因为可能有多个饥饿的goroutine）。 但是，如果在发现新工作后我们也观察到没有空闲Ps
	// ，可以暂停当前线程：系统已满载，因此不需要线程自旋。 另请参阅文件顶部的“工作线程park/unparking”注释。
	wasSpinning := _g_.m.spinning
	// M取消自旋状态
	if _g_.m.spinning {
		_g_.m.spinning = false
		if int32(atomic.Xadd(&sched.nmspinning, -1)) < 0 {
			throw("findrunnable: negative nmspinning")
		}
	}

	// check all runqueues once again
	// 再次检查所有的P，有没有可以运行的G
	for _, _p_ := range allpSnapshot {
		// 如果p的本地队列有G
		if !runqempty(_p_) {
			lock(&sched.lock)
			// 获取另外一个空闲P
			_p_ = pidleget()
			unlock(&sched.lock)
			if _p_ != nil {
				// 如果P不是nil，将M绑定P
				acquirep(_p_)
				// 如果是自旋，设置M为自旋
				if wasSpinning {
					_g_.m.spinning = true
					atomic.Xadd(&sched.nmspinning, 1)
				}
				// 返回到函数开头，从本地p获取G
				goto top
			}
			break
		}
	}

	// Check for idle-priority GC work again.
	if gcBlackenEnabled != 0 && gcMarkWorkAvailable(nil) {
		lock(&sched.lock)
		_p_ = pidleget()
		if _p_ != nil && _p_.gcBgMarkWorker == 0 {
			pidleput(_p_)
			_p_ = nil
		}
		unlock(&sched.lock)
		if _p_ != nil {
			acquirep(_p_)
			if wasSpinning {
				_g_.m.spinning = true
				atomic.Xadd(&sched.nmspinning, 1)
			}
			// Go back to idle GC check.
			goto stop
		}
	}

	// poll network
	// 再次检查netpoll
	if netpollinited() && atomic.Load(&netpollWaiters) > 0 && atomic.Xchg64(&sched.lastpoll, 0) != 0 {
		if _g_.m.p != 0 {
			throw("findrunnable: netpoll with p")
		}
		if _g_.m.spinning {
			throw("findrunnable: netpoll with spinning")
		}
		gp := netpoll(true) // block until new work is available
		atomic.Store64(&sched.lastpoll, uint64(nanotime()))
		if gp != nil {
			lock(&sched.lock)
			_p_ = pidleget()
			unlock(&sched.lock)
			if _p_ != nil {
				acquirep(_p_)
				injectglist(gp.schedlink.ptr())
				casgstatus(gp, _Gwaiting, _Grunnable)
				if trace.enabled {
					traceGoUnpark(gp, 0)
				}
				return gp, false
			}
			injectglist(gp)
		}
	}
	// 实在找不到G，那就休眠吧
	// 且此时的M一定不是自旋状态
	stopm()
	goto top
}

// pollWork returns true if there is non-background work this P could
// be doing. This is a fairly lightweight check to be used for
// background work loops, like idle GC. It checks a subset of the
// conditions checked by the actual scheduler.
// 检查当前的P是否有非后台工作，有则返回true
func pollWork() bool {
	// 全局队列有G，返回true
	if sched.runqsize != 0 {
		return true
	}
	p := getg().m.p.ptr()
	// P本地队列有G，返回true
	if !runqempty(p) {
		return true
	}
	// 如果有网络io的G，返回true
	if netpollinited() && atomic.Load(&netpollWaiters) > 0 && sched.lastpoll != 0 {
		if gp := netpoll(false); gp != nil {
			injectglist(gp)
			return true
		}
	}
	return false
}

// 重置m的为非自旋状态
// 唤醒或新建一个M来执行
func resetspinning() {
	_g_ := getg()
	if !_g_.m.spinning {
		throw("resetspinning: not a spinning m")
	}
	_g_.m.spinning = false
	// 将自旋的M个数减一
	nmspinning := atomic.Xadd(&sched.nmspinning, -1)
	if int32(nmspinning) < 0 {
		throw("findrunnable: negative nmspinning")
	}
	// M wakeup policy is deliberately somewhat conservative, so check if we
	// need to wakeup another P here. See "Worker thread parking/unparking"
	// comment at the top of the file for details.
	// 如果当前有空闲的P, 但是无自旋的M(nmspinning等于0), 则唤醒或新建一个M
	// 意思就是当前其他M要么很忙，要么在睡大觉，而且P有剩余，那么如果有睡大觉的M
	// 则唤醒它，没有的话就新建一个
	if nmspinning == 0 && atomic.Load(&sched.npidle) > 0 {
		wakep()
	}
}

// Injects the list of runnable G's into the scheduler.
// Can run concurrently with GC.
// 插入G的list到全局队列
func injectglist(glist *g) {
	if glist == nil {
		return
	}
	if trace.enabled {
		for gp := glist; gp != nil; gp = gp.schedlink.ptr() {
			traceGoUnpark(gp, 0)
		}
	}
	lock(&sched.lock)
	var n int
	for n = 0; glist != nil; n++ {
		gp := glist
		glist = gp.schedlink.ptr()
		casgstatus(gp, _Gwaiting, _Grunnable)
		globrunqput(gp)
	}
	unlock(&sched.lock)
	for ; n != 0 && sched.npidle != 0; n-- {
		startm(nil, false)
	}
}

// One round of scheduler: find a runnable goroutine and execute it.
// Never returns.
// 启动调度，尽力找到可运行的g去运行
// 1. 如果当前GC需要停止整个世界（STW), 则调用gcstopm休眠当前的M
// 2. 如果当前GC正在标记阶段, 则查找有没有待运行的GCWorker, GCWorker也是一个G
// 3. 每隔61次调度轮回从全局队列找
// 4. 从p.runnext获取g，从p的本地队列中获取
// 5. 调用findrunnable找G，找不到就不返回了，（详细流程看findrunnable注释）
func schedule() {
	_g_ := getg()

	if _g_.m.locks != 0 {
		throw("schedule: holding locks")
	}

	// 如果当前M锁定了某个G，那么应该交出P，进入休眠
	// 等待某个M调度拿到lockedg，然后唤醒lockedg的M
	// 具体看 startlockedm
	if _g_.m.lockedg != 0 {
		stoplockedm()
		// 执行M锁定的G，表示M和G锁定后，这个M就一直运行，直到lockedg退出
		execute(_g_.m.lockedg.ptr(), false) // Never returns.
	}

	// We should not schedule away from a g that is executing a cgo call,
	// since the cgo call is using the m's g0 stack.
	if _g_.m.incgo {
		throw("schedule: in cgo")
	}

top:
	// 如果当前GC需要停止整个世界（STW), 则调用gcstopm休眠当前的M
	if sched.gcwaiting != 0 {
		// 为了STW，停止当前的M
		gcstopm()
		// STW结束后回到 top
		goto top
	}

	// 如果M拥有的P中指定了需要在安全点运行的函数(P.runSafePointFn), 则运行它
	if _g_.m.p.ptr().runSafePointFn != 0 {
		runSafePointFn()
	}

	var gp *g
	var inheritTime bool

	// 开启了tools trace，则标记事件traceEvGoUnblock
	if trace.enabled || trace.shutdown {
		gp = traceReader()
		if gp != nil {
			casgstatus(gp, _Gwaiting, _Grunnable)
			traceGoUnpark(gp, 0)
		}
	}

	// 如果当前GC正在标记阶段, 则查找有没有待运行的GC Worker, GC Worker也是一个G
	// gcBlackenEnabled 在gc mark阶段会被置为1
	if gp == nil && gcBlackenEnabled != 0 {
		gp = gcController.findRunnableGCWorker(_g_.m.p.ptr())
	}

	// 以下都是想方设法找到可以运行的g，按照以下的顺序
	// 1. 每隔61次调度轮回从全局队列找
	// 2. 从p.runnext获取g，从p的本地队列中获取
	// 3. 尝试从全局队列中获取G
	// 4. 从网络IO轮询器中找到就绪的G，把这个G变为可运行的G
	if gp == nil {
		// Check the global runnable queue once in a while to ensure fairness.
		// Otherwise two goroutines can completely occupy the local runqueue
		// by constantly respawning each other.
		// 每隔61次调度，尝试从全局队列种获取G
		// ? 为何是61次？ https://github.com/golang/go/issues/20168
		if _g_.m.p.ptr().schedtick%61 == 0 && sched.runqsize > 0 {
			lock(&sched.lock)
			gp = globrunqget(_g_.m.p.ptr(), 1)
			unlock(&sched.lock)
		}
	}
	if gp == nil {
		// 从p的本地队列中获取
		gp, inheritTime = runqget(_g_.m.p.ptr())
		if gp != nil && _g_.m.spinning {
			throw("schedule: spinning with local work")
		}
	}
	if gp == nil {
		// 想尽办法找到可运行的G，找不到就不用返回了
		gp, inheritTime = findrunnable() // blocks until work is available
	}

	// This thread is going to run a goroutine and is not spinning anymore,
	// so if it was marked as spinning we need to reset it now and potentially
	// start a new spinning M.
	// M即将要执行G，如果M还是spinning，那么重置为false
	if _g_.m.spinning {
		// 重置为非自旋，并根据需要唤醒或新建一个M来运行
		resetspinning()
	}

	// 如果找到的G已经锁定M了，dolockOSThread和cgo会将G和M绑定
	// 则用startlockedm执行，将P和G都交给对方lockedm，唤醒绑定M-lockedm，自己回空闲队列。
	if gp.lockedm != 0 {
		// Hands off own p to the locked m,
		// then blocks waiting for a new p.
		startlockedm(gp)
		// 唤醒后回到头部重新获取任务。
		goto top
	}

	// println("execute goroutine", gp.goid)
	// 找到了g，那就执行g上的任务函数
	execute(gp, inheritTime)
}

// dropg removes the association between m and the current goroutine m->curg (gp for short).
// Typically a caller sets gp's status away from Grunning and then
// immediately calls dropg to finish the job. The caller is also responsible
// for arranging that gp will be restarted using ready at an
// appropriate time. After calling dropg and arranging for gp to be
// readied later, the caller can do other work but eventually should
// call schedule to restart the scheduling of goroutines on this m.
//
// dropg删除m与当前goroutine m-> curg（简称gp）之间的关联。
// 通常，调用者将gp的状态设置为远离Grunning，然后立即调用dropg完成作业。
// 调用者还负责安排gp将在适当的时间使用ready重新启动。
// 在调用dropg并安排gp稍后准备好之后，调用者可以做其他工作，
// 但最终应调用schedule来重新启动此m上的goroutine的调度。
//
// dropg实现把M和当前G的关联移除
// 通常调用者将状态Grunning移除的时候会立即调用dropg
func dropg() {
	_g_ := getg()

	setMNoWB(&_g_.m.curg.m, nil)
	setGNoWB(&_g_.m.curg, nil)
}

func parkunlock_c(gp *g, lock unsafe.Pointer) bool {
	unlock((*mutex)(lock))
	return true
}

// park continuation on g0.
// 让G休眠
func park_m(gp *g) {
	_g_ := getg()

	if trace.enabled {
		traceGoPark(_g_.m.waittraceev, _g_.m.waittraceskip)
	}
	// 设置当前状态从Grunning-->Gwaiting
	casgstatus(gp, _Grunning, _Gwaiting)
	// 当前g放弃m
	dropg()

	// 执行解锁函数，如果返回为false，则恢复执行
	if _g_.m.waitunlockf != nil {
		fn := *(*func(*g, unsafe.Pointer) bool)(unsafe.Pointer(&_g_.m.waitunlockf))
		ok := fn(gp, _g_.m.waitlock)
		_g_.m.waitunlockf = nil
		_g_.m.waitlock = nil
		if !ok {
			if trace.enabled {
				traceGoUnpark(gp, 2)
			}
			casgstatus(gp, _Gwaiting, _Grunnable)
			execute(gp, true) // Schedule it back, never returns.
		}
	}
	// 调度执行其他任务
	schedule()
}

// 将当前的G入全剧队列，然后调用调度器
func goschedImpl(gp *g) {
	status := readgstatus(gp)
	if status&^_Gscan != _Grunning {
		dumpgstatus(gp)
		throw("bad g status")
	}
	// 将gp的状态改为_Grunnable
	casgstatus(gp, _Grunning, _Grunnable)
	// 解除与当前M的关联
	dropg()
	lock(&sched.lock)
	// 入全局队列
	globrunqput(gp)
	unlock(&sched.lock)
	// 启动调度
	schedule()
}

// Gosched continuation on g0.
func gosched_m(gp *g) {
	if trace.enabled {
		traceGoSched()
	}
	goschedImpl(gp)
}

// goschedguarded is a forbidden-states-avoided version of gosched_m
func goschedguarded_m(gp *g) {

	if gp.m.locks != 0 || gp.m.mallocing != 0 || gp.m.preemptoff != "" || gp.m.p.ptr().status != _Prunning {
		gogo(&gp.sched) // never return
	}

	if trace.enabled {
		traceGoSched()
	}
	goschedImpl(gp)
}

// 和gosched_m的作用是一样的
func gopreempt_m(gp *g) {
	if trace.enabled {
		traceGoPreempt()
	}
	goschedImpl(gp)
}

// Finishes execution of the current goroutine.
// 当goroutine结束后，会调用这个函数
func goexit1() {
	if raceenabled {
		racegoend()
	}
	// goroutine trace 结束记录
	if trace.enabled {
		traceGoEnd()
	}
	// 切换到g0执行goexit0
	mcall(goexit0)
}

// goexit continuation on g0.
func goexit0(gp *g) {
	_g_ := getg()

	// gp的状态置为_Gdead
	casgstatus(gp, _Grunning, _Gdead)
	// 如果runtime内部goroutine ngsys 减1
	if isSystemGoroutine(gp) {
		atomic.Xadd(&sched.ngsys, -1)
	}
	// 状态重置
	gp.m = nil
	// G和M是否锁定
	locked := gp.lockedm != 0
	// G和M解除锁定
	gp.lockedm = 0
	_g_.m.lockedg = 0

	gp.paniconfault = false
	gp._defer = nil // should be true already but just in case.
	gp._panic = nil // non-nil for Goexit during panic. points at stack-allocated data.
	gp.writebuf = nil
	gp.waitreason = ""
	gp.param = nil
	gp.labels = nil
	gp.timer = nil

	if gcBlackenEnabled != 0 && gp.gcAssistBytes > 0 {
		// Flush assist credit to the global pool. This gives
		// better information to pacing if the application is
		// rapidly creating an exiting goroutines.
		scanCredit := int64(gcController.assistWorkPerByte * float64(gp.gcAssistBytes))
		atomic.Xaddint64(&gcController.bgScanCredit, scanCredit)
		gp.gcAssistBytes = 0
	}

	// Note that gp's stack scan is now "valid" because it has no
	// stack.
	gp.gcscanvalid = true
	// 处理G和M的清除工作
	dropg()

	if _g_.m.lockedInt != 0 {
		print("invalid m->lockedInt = ", _g_.m.lockedInt, "\n")
		throw("internal lockOSThread error")
	}
	_g_.m.lockedExt = 0
	// 将G放入P的G空闲链表
	gfput(_g_.m.p.ptr(), gp)
	if locked {
		// The goroutine may have locked this thread because
		// it put it in an unusual kernel state. Kill it
		// rather than returning it to the thread pool.

		// Return to mstart, which will release the P and exit
		// the thread.
		if GOOS != "plan9" { // See golang.org/issue/22227.
			gogo(&_g_.m.g0.sched)
		}
	}
	// 再次计入调度
	schedule()
}

// save updates getg().sched to refer to pc and sp so that a following
// gogo will restore pc and sp.
//
// save must not have write barriers because invoking a write barrier
// can clobber getg().sched.
//
//go:nosplit
//go:nowritebarrierrec
// save是保存pc和sp到当前g的运行现场
func save(pc, sp uintptr) {
	_g_ := getg()

	_g_.sched.pc = pc
	_g_.sched.sp = sp
	_g_.sched.lr = 0
	_g_.sched.ret = 0
	_g_.sched.g = guintptr(unsafe.Pointer(_g_))
	// We need to ensure ctxt is zero, but can't have a write
	// barrier here. However, it should always already be zero.
	// Assert that.
	if _g_.sched.ctxt != nil {
		badctxt()
	}
}

// The goroutine g is about to enter a system call.
// Record that it's not using the cpu anymore.
// This is called only from the go syscall library and cgocall,
// not from the low-level system calls used by the runtime.
//
// Entersyscall cannot split the stack: the gosave must
// make g->sched refer to the caller's stack segment, because
// entersyscall is going to return immediately after.
//
// Nothing entersyscall calls can split the stack either.
// We cannot safely move the stack during an active call to syscall,
// because we do not know which of the uintptr arguments are
// really pointers (back into the stack).
// In practice, this means that we make the fast path run through
// entersyscall doing no-split things, and the slow path has to use systemstack
// to run bigger things on the system stack.
//
// reentersyscall is the entry point used by cgo callbacks, where explicitly
// saved SP and PC are restored. This is needed when exitsyscall will be called
// from a function further up in the call stack than the parent, as g->syscallsp
// must always point to a valid stack frame. entersyscall below is the normal
// entry point for syscalls, which obtains the SP and PC from the caller.
//
// Syscall tracing:
// At the start of a syscall we emit traceGoSysCall to capture the stack trace.
// If the syscall does not block, that is it, we do not emit any other events.
// If the syscall blocks (that is, P is retaken), retaker emits traceGoSysBlock;
// when syscall returns we emit traceGoSysExit and when the goroutine starts running
// (potentially instantly, if exitsyscallfast returns true) we emit traceGoStart.
// To ensure that traceGoSysExit is emitted strictly after traceGoSysBlock,
// we remember current value of syscalltick in m (_g_.m.syscalltick = _g_.m.p.ptr().syscalltick),
// whoever emits traceGoSysBlock increments p.syscalltick afterwards;
// and we wait for the increment before emitting traceGoSysExit.
// Note that the increment is done even if tracing is not enabled,
// because tracing can be enabled in the middle of syscall. We don't want the wait to hang.
//
//go:nosplit
//
// goroutine g即将进入系统调用。记录它不再使用cpu。
// 这只是来自go syscall库和cgocall，而不是来自运行时使用的低级系统调用.
// Entersyscall无法拆分堆栈：gosave mustmake g-> sched指的是调用者的堆栈段，
// 因为aftercysyscall将立即返回。没有任何enteryscall调用可以拆分堆栈。
// 我们无法安全地移动堆栈在系统调用的活动期间，
// 因为我们不知道哪个uintptr参数实际上是指针（回到堆栈中）。
// 实际上，这意味着我们通过快速路径运行来执行无拆分事务，
// 慢速路径必须使用systemstack在系统堆栈上运行更大的东西.
// reentersyscall是cgo回调使用的入口点，其中明确保存的SP和PC已恢复。
// 当从调用堆栈中的函数调用exitsyscall而不是父函数时，需要这样做，
// 因为g-> syscallspmust总是指向有效的堆栈帧。
// 下面的enteryscall是系统调用的normalentry点，
// 它从调用者获取SP和PC.Syscall跟踪：在系统调用开始时，
// 我们发出traceGoSysCall来捕获堆栈跟踪。
// 如果系统调用没有阻塞，就是它，我们这样做不发出任何其他事件。
// 如果系统调用块（即重新获得P），则重新发出traceGoSysBlock;当syscall返回时，
// 我们发出traceGoSysExit，当goroutine开始运行时（可能立即，如果exitsyscallfast返回true），
// 我们发出traceGoStart.To确保traceGoSysExit是在traceGoSysBlock之后严格发出的，
// 我们记得m中的syscalltick的当前值（_g_.m.syscalltick = _g_.mpptr（）。
// syscalltick），之后发出traceGoSysBlock的人会增加p.
// syscalltick;我们等待发送之前的增量traceGoSysExit。
// 请注意，即使未启用跟踪，也会完成增量，因为可以在系统调用中启用跟踪。我们不希望等待挂起。
func reentersyscall(pc, sp uintptr) {
	_g_ := getg()

	// Disable preemption because during this function g is in Gsyscall status,
	// but can have inconsistent g->sched, do not let GC observe it.
	//禁用抢占，因为在此函数期间g处于Gsyscall状态，但可能具有不一致的g->sched，请不要让GC观察它
	_g_.m.locks++

	// Entersyscall must not call any function that might split/grow the stack.
	// (See details in comment above.)
	// Catch calls that might, by replacing the stack guard with something that
	// will trip any stack check and leaving a flag to tell newstack to die.
	// Entersyscall不得调用任何可能拆分/增长堆栈的函数。（请参阅上面的注释中的详细信息。）
	// 通过将堆栈保护替换为任何堆栈检查并留下标志告诉newstack死亡，可能会捕获调用
	_g_.stackguard0 = stackPreempt
	_g_.throwsplit = true

	// Leave SP around for GC and traceback.
	save(pc, sp)
	_g_.syscallsp = sp
	_g_.syscallpc = pc
	// 让G进入_Gsyscall状态，此时G已经被挂起了，直到系统调用结束，才会让G重新进入running
	casgstatus(_g_, _Grunning, _Gsyscall)
	// 检查栈是否超出
	if _g_.syscallsp < _g_.stack.lo || _g_.stack.hi < _g_.syscallsp {
		systemstack(func() {
			print("entersyscall inconsistent ", hex(_g_.syscallsp), " [", hex(_g_.stack.lo), ",", hex(_g_.stack.hi), "]\n")
			throw("entersyscall")
		})
	}

	// 开启了 trace 打印信息
	if trace.enabled {
		systemstack(traceGoSysCall)
		// systemstack itself clobbers g.sched.{pc,sp} and we might
		// need them later when the G is genuinely blocked in a
		// syscall
		save(pc, sp)
	}

	if atomic.Load(&sched.sysmonwait) != 0 {
		systemstack(entersyscall_sysmon)
		save(pc, sp)
	}

	if _g_.m.p.ptr().runSafePointFn != 0 {
		// runSafePointFn may stack split if run on this stack
		systemstack(runSafePointFn)
		save(pc, sp)
	}

	_g_.m.syscalltick = _g_.m.p.ptr().syscalltick
	_g_.sysblocktraced = true
	// 这里很关键：P的M已经陷入系统调用，于是P忍痛放弃该M
	// 但是请注意：此时M还指向P，在M从系统调用返回后还能找到P
	_g_.m.mcache = nil
	_g_.m.p.ptr().m = 0
	// P的状态变为Psyscall
	atomic.Store(&_g_.m.p.ptr().status, _Psyscall)
	if sched.gcwaiting != 0 {
		systemstack(entersyscall_gcwait)
		save(pc, sp)
	}

	// Goroutines must not split stacks in Gsyscall status (it would corrupt g->sched).
	// We set _StackGuard to StackPreempt so that first split stack check calls morestack.
	// Morestack detects this case and throws.
	_g_.stackguard0 = stackPreempt
	_g_.m.locks--
}

// Standard syscall entry used by the go syscall library and normal cgo calls.
//go:nosplit
// syscall/asm_linux_amd64.s
// 	TEXT	·Syscall(SB),NOSPLIT,$0-56
// 		CALL	runtime·entersyscall(SB)
// 		....
//		CALL	runtime·exitsyscall(SB)
// 系统调用的时候调用该函数
// 进入系统调用，G将会进入_Gsyscall状态，也就是会被暂时挂起，直到系统调用结束。
// 此时M进入系统调用，那么P也会放弃该M。但是，此时M还指向P，在M从系统调用返回后还能找到P
func entersyscall(dummy int32) {
	reentersyscall(getcallerpc(), getcallersp(unsafe.Pointer(&dummy)))
}

func entersyscall_sysmon() {
	lock(&sched.lock)
	if atomic.Load(&sched.sysmonwait) != 0 {
		atomic.Store(&sched.sysmonwait, 0)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
}

func entersyscall_gcwait() {
	_g_ := getg()
	_p_ := _g_.m.p.ptr()

	lock(&sched.lock)
	if sched.stopwait > 0 && atomic.Cas(&_p_.status, _Psyscall, _Pgcstop) {
		if trace.enabled {
			traceGoSysBlock(_p_)
			traceProcStop(_p_)
		}
		_p_.syscalltick++
		if sched.stopwait--; sched.stopwait == 0 {
			notewakeup(&sched.stopnote)
		}
	}
	unlock(&sched.lock)
}

// The same as entersyscall(), but with a hint that the syscall is blocking.
//go:nosplit
// 知道自己会 block，直接就把 p 交出来了。
func entersyscallblock(dummy int32) {
	_g_ := getg()

	_g_.m.locks++ // see comment in entersyscall
	_g_.throwsplit = true
	_g_.stackguard0 = stackPreempt // see comment in entersyscall
	_g_.m.syscalltick = _g_.m.p.ptr().syscalltick
	_g_.sysblocktraced = true
	_g_.m.p.ptr().syscalltick++

	// Leave SP around for GC and traceback.
	pc := getcallerpc()
	sp := getcallersp(unsafe.Pointer(&dummy))
	save(pc, sp)
	_g_.syscallsp = _g_.sched.sp
	_g_.syscallpc = _g_.sched.pc
	if _g_.syscallsp < _g_.stack.lo || _g_.stack.hi < _g_.syscallsp {
		sp1 := sp
		sp2 := _g_.sched.sp
		sp3 := _g_.syscallsp
		systemstack(func() {
			print("entersyscallblock inconsistent ", hex(sp1), " ", hex(sp2), " ", hex(sp3), " [", hex(_g_.stack.lo), ",", hex(_g_.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}
	casgstatus(_g_, _Grunning, _Gsyscall)
	if _g_.syscallsp < _g_.stack.lo || _g_.stack.hi < _g_.syscallsp {
		systemstack(func() {
			print("entersyscallblock inconsistent ", hex(sp), " ", hex(_g_.sched.sp), " ", hex(_g_.syscallsp), " [", hex(_g_.stack.lo), ",", hex(_g_.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}

	systemstack(entersyscallblock_handoff)

	// Resave for traceback during blocked call.
	save(getcallerpc(), getcallersp(unsafe.Pointer(&dummy)))

	_g_.m.locks--
}

func entersyscallblock_handoff() {
	if trace.enabled {
		traceGoSysCall()
		traceGoSysBlock(getg().m.p.ptr())
	}
	handoffp(releasep())
}

// The goroutine g exited its system call.
// Arrange for it to run on a cpu again.
// This is called only from the go syscall library, not
// from the low-level system calls used by the runtime.
//
// Write barriers are not allowed because our P may have been stolen.
//
//go:nosplit
//go:nowritebarrierrec
// goroutine g退出系统调用。安排它再次在cpu上运行。
// 这个函数只能从go syscall库中调用，而不是从运行时使用的低级系统调用中调用
func exitsyscall(dummy int32) {
	_g_ := getg()

	// 和进入系统调用一样，禁用抢占
	_g_.m.locks++ // see comment in entersyscall
	if getcallersp(unsafe.Pointer(&dummy)) > _g_.syscallsp {
		// throw calls print which may try to grow the stack,
		// but throwsplit == true so the stack can not be grown;
		// use systemstack to avoid that possible problem.
		systemstack(func() {
			throw("exitsyscall: syscall frame is no longer valid")
		})
	}

	_g_.waitsince = 0
	oldp := _g_.m.p.ptr()
	// 快速路径处理，判断
	if exitsyscallfast() {
		if _g_.m.mcache == nil {
			systemstack(func() {
				throw("lost mcache")
			})
		}
		if trace.enabled {
			if oldp != _g_.m.p.ptr() || _g_.m.syscalltick != _g_.m.p.ptr().syscalltick {
				systemstack(traceGoStart)
			}
		}
		// There's a cpu for us, so we can run.
		_g_.m.p.ptr().syscalltick++
		// We need to cas the status and scan before resuming...
		// g的状态从syscall变成running，这样M就可以找到这个g来运行，
		// 正常来说，g很快就能被运行
		casgstatus(_g_, _Gsyscall, _Grunning)

		// Garbage collector isn't running (since we are),
		// so okay to clear syscallsp.
		_g_.syscallsp = 0
		_g_.m.locks--
		if _g_.preempt {
			// restore the preemption request in case we've cleared it in newstack
			_g_.stackguard0 = stackPreempt
		} else {
			// otherwise restore the real _StackGuard, we've spoiled it in entersyscall/entersyscallblock
			_g_.stackguard0 = _g_.stack.lo + _StackGuard
		}
		_g_.throwsplit = false
		return
	}

	_g_.sysexitticks = 0
	if trace.enabled {
		// Wait till traceGoSysBlock event is emitted.
		// This ensures consistency of the trace (the goroutine is started after it is blocked).
		for oldp != nil && oldp.syscalltick == _g_.m.syscalltick {
			osyield()
		}
		// We can't trace syscall exit right now because we don't have a P.
		// Tracing code can invoke write barriers that cannot run without a P.
		// So instead we remember the syscall exit time and emit the event
		// in execute when we have a P.
		_g_.sysexitticks = cputicks()
	}

	_g_.m.locks--

	// Call the scheduler.
	// 前面就是努力获取一个 P 来执行 syscall 之后的逻辑。
	// 如果哪都没有 P 可以给我们用，那就进入 exitsyscall0 了。
	// 调用 exitsyscall0 时，会切换到 g0 栈。
	mcall(exitsyscall0)

	if _g_.m.mcache == nil {
		systemstack(func() {
			throw("lost mcache")
		})
	}

	// Scheduler returned, so we're allowed to run now.
	// Delete the syscallsp information that we left for
	// the garbage collector during the system call.
	// Must wait until now because until gosched returns
	// we don't know for sure that the garbage collector
	// is not running.
	_g_.syscallsp = 0
	_g_.m.p.ptr().syscalltick++
	_g_.throwsplit = false
}

//go:nosplit
func exitsyscallfast() bool {
	_g_ := getg()

	// Freezetheworld sets stopwait but does not retake P's.
	if sched.stopwait == freezeStopWait {
		_g_.m.mcache = nil
		_g_.m.p = 0
		return false
	}

	// Try to re-acquire the last P.
	// 如果之前附属的P尚未被其他M,尝试绑定该P
	if _g_.m.p != 0 && _g_.m.p.ptr().status == _Psyscall && atomic.Cas(&_g_.m.p.ptr().status, _Psyscall, _Prunning) {
		// There's a cpu for us, so we can run.
		exitsyscallfast_reacquired()
		return true
	}

	// Try to get any other idle P.
	// 否则从空闲P列表中取出一个来
	oldp := _g_.m.p.ptr()
	_g_.m.mcache = nil
	_g_.m.p = 0
	if sched.pidle != 0 {
		var ok bool
		systemstack(func() {
			ok = exitsyscallfast_pidle()
			if ok && trace.enabled {
				if oldp != nil {
					// Wait till traceGoSysBlock event is emitted.
					// This ensures consistency of the trace (the goroutine is started after it is blocked).
					for oldp.syscalltick == _g_.m.syscalltick {
						osyield()
					}
				}
				traceGoSysExit(0)
			}
		})
		if ok {
			return true
		}
	}
	return false
}

// exitsyscallfast_reacquired is the exitsyscall path on which this G
// has successfully reacquired the P it was running on before the
// syscall.
//
// This function is allowed to have write barriers because exitsyscall
// has acquired a P at this point.
//
//go:yeswritebarrierrec
//go:nosplit
func exitsyscallfast_reacquired() {
	_g_ := getg()
	_g_.m.mcache = _g_.m.p.ptr().mcache
	_g_.m.p.ptr().m.set(_g_.m)
	if _g_.m.syscalltick != _g_.m.p.ptr().syscalltick {
		if trace.enabled {
			// The p was retaken and then enter into syscall again (since _g_.m.syscalltick has changed).
			// traceGoSysBlock for this syscall was already emitted,
			// but here we effectively retake the p from the new syscall running on the same p.
			systemstack(func() {
				// Denote blocking of the new syscall.
				traceGoSysBlock(_g_.m.p.ptr())
				// Denote completion of the current syscall.
				traceGoSysExit(0)
			})
		}
		_g_.m.p.ptr().syscalltick++
	}
}

func exitsyscallfast_pidle() bool {
	lock(&sched.lock)
	_p_ := pidleget()
	if _p_ != nil && atomic.Load(&sched.sysmonwait) != 0 {
		atomic.Store(&sched.sysmonwait, 0)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	if _p_ != nil {
		acquirep(_p_)
		return true
	}
	return false
}

// exitsyscall slow path on g0.
// Failed to acquire P, enqueue gp as runnable.
//
//go:nowritebarrierrec
func exitsyscall0(gp *g) {
	_g_ := getg()

	casgstatus(gp, _Gsyscall, _Grunnable)
	dropg()
	lock(&sched.lock)
	_p_ := pidleget()
	if _p_ == nil {
		globrunqput(gp)
	} else if atomic.Load(&sched.sysmonwait) != 0 {
		atomic.Store(&sched.sysmonwait, 0)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	if _p_ != nil {
		acquirep(_p_)
		execute(gp, false) // Never returns.
	}
	if _g_.m.lockedg != 0 {
		// Wait until another thread schedules gp and so m again.
		stoplockedm()
		execute(gp, false) // Never returns.
	}
	stopm()
	schedule() // Never returns.
}

func beforefork() {
	gp := getg().m.curg

	// Block signals during a fork, so that the child does not run
	// a signal handler before exec if a signal is sent to the process
	// group. See issue #18600.
	gp.m.locks++
	msigsave(gp.m)
	sigblock()

	// This function is called before fork in syscall package.
	// Code between fork and exec must not allocate memory nor even try to grow stack.
	// Here we spoil g->_StackGuard to reliably detect any attempts to grow stack.
	// runtime_AfterFork will undo this in parent process, but not in child.
	gp.stackguard0 = stackFork
}

// Called from syscall package before fork.
//go:linkname syscall_runtime_BeforeFork syscall.runtime_BeforeFork
//go:nosplit
func syscall_runtime_BeforeFork() {
	systemstack(beforefork)
}

func afterfork() {
	gp := getg().m.curg

	// See the comments in beforefork.
	gp.stackguard0 = gp.stack.lo + _StackGuard

	msigrestore(gp.m.sigmask)

	gp.m.locks--
}

// Called from syscall package after fork in parent.
//go:linkname syscall_runtime_AfterFork syscall.runtime_AfterFork
//go:nosplit
func syscall_runtime_AfterFork() {
	systemstack(afterfork)
}

// inForkedChild is true while manipulating signals in the child process.
// This is used to avoid calling libc functions in case we are using vfork.
var inForkedChild bool

// Called from syscall package after fork in child.
// It resets non-sigignored signals to the default handler, and
// restores the signal mask in preparation for the exec.
//
// Because this might be called during a vfork, and therefore may be
// temporarily sharing address space with the parent process, this must
// not change any global variables or calling into C code that may do so.
//
//go:linkname syscall_runtime_AfterForkInChild syscall.runtime_AfterForkInChild
//go:nosplit
//go:nowritebarrierrec
func syscall_runtime_AfterForkInChild() {
	// It's OK to change the global variable inForkedChild here
	// because we are going to change it back. There is no race here,
	// because if we are sharing address space with the parent process,
	// then the parent process can not be running concurrently.
	inForkedChild = true

	clearSignalHandlers()

	// When we are the child we are the only thread running,
	// so we know that nothing else has changed gp.m.sigmask.
	msigrestore(getg().m.sigmask)

	inForkedChild = false
}

// Called from syscall package before Exec.
//go:linkname syscall_runtime_BeforeExec syscall.runtime_BeforeExec
func syscall_runtime_BeforeExec() {
	// Prevent thread creation during exec.
	execLock.lock()
}

// Called from syscall package after Exec.
//go:linkname syscall_runtime_AfterExec syscall.runtime_AfterExec
func syscall_runtime_AfterExec() {
	execLock.unlock()
}

// Allocate a new g, with a stack big enough for stacksize bytes.
// 新分配一个g对象，并申请好 stacksize 大小的栈，此时g的状态为_Gidle
// 初始栈的分布：
/*

 */
func malg(stacksize int32) *g {
	newg := new(g)
	// print("newg: ", newg)
	if stacksize >= 0 {
		stacksize = round2(_StackSystem + stacksize)
		systemstack(func() {
			// 调用 stackalloc 分配栈
			newg.stack = stackalloc(uint32(stacksize))
		})
		// 设置 stackguard
		newg.stackguard0 = newg.stack.lo + _StackGuard
		newg.stackguard1 = ^uintptr(0)
	}
	return newg
}

// Create a new g running fn with siz bytes of arguments.
// Put it on the queue of g's waiting to run.
// The compiler turns a go statement into a call to this.
// Cannot split the stack because it assumes that the arguments
// are available sequentially after &fn; they would not be
// copied if a stack split occurred.
//go:nosplit
// 新建一个goroutine，
/* 参考go源码阅读笔记
lower
			SP +------------+
			   |			|
^              +------------+  <----  newproc frame
. 			   |    pc/ip   |
.              +------------+
. 			   |    siz 	|
address		   +------------+ ---+
. 			   |   add		|	 |
.              +------------+    |
. 			   |     x		|	  >fn
.              +------------+    |
        	   |	y		|	 |
higher		   +------------+ ---+
*/
// 用fn + PtrSize 获取第一个参数的地址，也就是argp
// 用siz - 8 获取pc地址
func newproc(siz int32, fn *funcval) {
	argp := add(unsafe.Pointer(&fn), sys.PtrSize)
	pc := getcallerpc()
	// 用g0的栈创建G对象
	systemstack(func() {
		newproc1(fn, (*uint8)(argp), siz, pc)
	})
}

// Create a new g running fn with narg bytes of arguments starting
// at argp. callerpc is the address of the go statement that created
// this. The new g is put on the queue of g's waiting to run.
// 根据函数参数和函数地址，创建一个新的G，然后将这个G加入队列等待运行
// callerpc是newproc函数的pc
func newproc1(fn *funcval, argp *uint8, narg int32, callerpc uintptr) {
	// print("fn=", fn.fn, " argp=", argp, " narg=", narg, " callerpc=", callerpc, "\n")
	_g_ := getg() // g0

	// 任务函数如果为nil，则抛出异常
	if fn == nil {
		_g_.m.throwing = -1 // do not dump full stacks
		throw("go of nil func value")
	}
	// 禁用抢占，因为它可以在本地var中保存p
	_g_.m.locks++ // disable preemption because it can be holding p in a local var
	siz := narg
	siz = (siz + 7) &^ 7

	// We could allocate a larger initial stack if necessary.
	// Not worth it: this is almost always an error.
	// 4*sizeof(uintreg): extra space added below
	// sizeof(uintreg): caller's LR (arm) or return address (x86, in gostartcall).
	// 如果函数的参数大小比2048大的话，直接panic
	if siz >= _StackMin-4*sys.RegSize-sys.RegSize {
		throw("newproc: function arguments too large for new goroutine")
	}

	// 从m中获取p
	_p_ := _g_.m.p.ptr()
	// 尝试从gfree list获取g，包括本地和全局list
	newg := gfget(_p_)
	// 如果没获取到g，则新建一个
	if newg == nil {
		// 分配栈为 2k 大小的G对象
		newg = malg(_StackMin)
		casgstatus(newg, _Gidle, _Gdead) //将g的状态改为_Gdead
		// 添加到allg数组，防止gc扫描清除掉
		allgadd(newg) // publishes with a g->status of Gdead so GC scanner doesn't look at uninitialized stack.
	}
	if newg.stack.hi == 0 {
		throw("newproc1: newg missing stack")
	}

	// 此时获取的g一定得是 _Gdead
	if readgstatus(newg) != _Gdead {
		throw("newproc1: new g is not Gdead")
	}

	// 参数大小+稍微一点空间
	totalSize := 4*sys.RegSize + uintptr(siz) + sys.MinFrameSize // extra space in case of reads slightly beyond frame
	totalSize += -totalSize & (sys.SpAlign - 1)                  // align to spAlign
	// 新协程的栈顶计算，将栈顶减去参数占用的空间
	sp := newg.stack.hi - totalSize
	spArg := sp
	if usesLR { // amd64平台下 usesLR 为 false
		// caller's LR
		*(*uintptr)(unsafe.Pointer(sp)) = 0
		prepGoExitFrame(sp)
		spArg += sys.MinFrameSize
	}

	// 如果有参数
	if narg > 0 {
		// copy参数到栈上
		memmove(unsafe.Pointer(spArg), unsafe.Pointer(argp), uintptr(narg))
		// This is a stack-to-stack copy. If write barriers
		// are enabled and the source stack is grey (the
		// destination is always black), then perform a
		// barrier copy. We do this *after* the memmove
		// because the destination stack may have garbage on
		// it.
		// 这是一个堆栈到堆栈的副本。如果启用了写入屏障并且源堆栈为灰色（目标始终为黑色），
		// 则执行屏障复制。我们在* memmove之后执行此操作，因为目标堆栈上可能有垃圾。
		if writeBarrier.needed && !_g_.m.curg.gcscandone {
			f := findfunc(fn.fn)
			stkmap := (*stackmap)(funcdata(f, _FUNCDATA_ArgsPointerMaps))
			// We're in the prologue, so it's always stack map index 0.
			bv := stackmapdata(stkmap, 0)
			bulkBarrierBitmap(spArg, spArg, uintptr(narg), 0, bv.bytedata)
		}
	}

	// 初始化G的gobuf，保存sp，pc，任务函数等
	memclrNoHeapPointers(unsafe.Pointer(&newg.sched), unsafe.Sizeof(newg.sched))
	newg.sched.sp = sp
	newg.stktopsp = sp
	// 保存goexit的地址到sched.pc，后面会调节 goexit 作为任务函数返回后执行的地址，所以goroutine结束后会调用goexit
	newg.sched.pc = funcPC(goexit) + sys.PCQuantum // +PCQuantum so that previous instruction is in same function
	// sched.g保存当前新的G
	newg.sched.g = guintptr(unsafe.Pointer(newg))
	// 将当前的pc压入栈，保存g的任务函数为pc
	gostartcallfn(&newg.sched, fn)
	// gopc保存newproc的pc
	newg.gopc = callerpc
	// 任务函数的地址
	newg.startpc = fn.fn
	if _g_.m.curg != nil {
		newg.labels = _g_.m.curg.labels
	}
	// 判断g的任务函数是不是runtime系统的任务函数，是则sched.ngsys加1
	if isSystemGoroutine(newg) {
		atomic.Xadd(&sched.ngsys, +1)
	}
	newg.gcscanvalid = false
	// 更改当前g的状态为_Grunnable
	casgstatus(newg, _Gdead, _Grunnable)

	if _p_.goidcache == _p_.goidcacheend {
		// Sched.goidgen is the last allocated id,
		// this batch must be [sched.goidgen+1, sched.goidgen+GoidCacheBatch].
		// At startup sched.goidgen=0, so main goroutine receives goid=1.
		_p_.goidcache = atomic.Xadd64(&sched.goidgen, _GoidCacheBatch)
		_p_.goidcache -= _GoidCacheBatch - 1
		_p_.goidcacheend = _p_.goidcache + _GoidCacheBatch
	}
	// 生成唯一的goid
	newg.goid = int64(_p_.goidcache)
	_p_.goidcache++
	if raceenabled {
		newg.racectx = racegostart(callerpc)
	}
	if trace.enabled {
		// 如果启动了go trace，记录go create事件
		traceGoCreate(newg, newg.startpc)
	}

	// println("new goroutine", newg.goid)
	// 将当前新生成的g，放入队列
	runqput(_p_, newg, true)

	// 如果有空闲的p 且 m没有处于自旋状态 且 main goroutine已经启动，那么唤醒某个m来执行任务
	if atomic.Load(&sched.npidle) != 0 && atomic.Load(&sched.nmspinning) == 0 && mainStarted {
		// 如果还有空闲P，那么新建M来运行G
		print("wakeup ", newg.goid, " ", sched.npidle, " ", sched.nmspinning, "\n")
		wakep()
	}
	_g_.m.locks--
	if _g_.m.locks == 0 && _g_.preempt { // restore the preemption request in case we've cleared it in newstack
		_g_.stackguard0 = stackPreempt
	}
}

// Put on gfree list.
// If local list is too long, transfer a batch to the global list.
func gfput(_p_ *p, gp *g) {
	if readgstatus(gp) != _Gdead {
		throw("gfput: bad status (not Gdead)")
	}

	stksize := gp.stack.hi - gp.stack.lo

	if stksize != _FixedStack {
		// non-standard stack size - free it.
		stackfree(gp.stack)
		gp.stack.lo = 0
		gp.stack.hi = 0
		gp.stackguard0 = 0
	}

	gp.schedlink.set(_p_.gfree)
	_p_.gfree = gp
	_p_.gfreecnt++
	if _p_.gfreecnt >= 64 {
		lock(&sched.gflock)
		for _p_.gfreecnt >= 32 {
			_p_.gfreecnt--
			gp = _p_.gfree
			_p_.gfree = gp.schedlink.ptr()
			if gp.stack.lo == 0 {
				gp.schedlink.set(sched.gfreeNoStack)
				sched.gfreeNoStack = gp
			} else {
				gp.schedlink.set(sched.gfreeStack)
				sched.gfreeStack = gp
			}
			sched.ngfree++
		}
		unlock(&sched.gflock)
	}
}

// Get from gfree list.
// If local list is empty, grab a batch from global list.
// 从p的本地空闲链表获取g，如果获取不到尝试从全局链表里获取
func gfget(_p_ *p) *g {
retry:
	// 本地g空闲链表
	gp := _p_.gfree
	if gp == nil && (sched.gfreeStack != nil || sched.gfreeNoStack != nil) {
		lock(&sched.gflock)
		for _p_.gfreecnt < 32 {
			if sched.gfreeStack != nil {
				// Prefer Gs with stacks.
				gp = sched.gfreeStack
				sched.gfreeStack = gp.schedlink.ptr()
			} else if sched.gfreeNoStack != nil {
				gp = sched.gfreeNoStack
				sched.gfreeNoStack = gp.schedlink.ptr()
			} else {
				break
			}
			_p_.gfreecnt++
			sched.ngfree--
			gp.schedlink.set(_p_.gfree)
			_p_.gfree = gp
		}
		unlock(&sched.gflock)
		goto retry
	}
	if gp != nil {
		_p_.gfree = gp.schedlink.ptr()
		_p_.gfreecnt--
		if gp.stack.lo == 0 {
			// Stack was deallocated in gfput. Allocate a new one.
			systemstack(func() {
				gp.stack = stackalloc(_FixedStack)
			})
			gp.stackguard0 = gp.stack.lo + _StackGuard
		} else {
			if raceenabled {
				racemalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
			}
			if msanenabled {
				msanmalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
			}
		}
	}
	return gp
}

// Purge all cached G's from gfree list to the global list.
func gfpurge(_p_ *p) {
	lock(&sched.gflock)
	for _p_.gfreecnt != 0 {
		_p_.gfreecnt--
		gp := _p_.gfree
		_p_.gfree = gp.schedlink.ptr()
		if gp.stack.lo == 0 {
			gp.schedlink.set(sched.gfreeNoStack)
			sched.gfreeNoStack = gp
		} else {
			gp.schedlink.set(sched.gfreeStack)
			sched.gfreeStack = gp
		}
		sched.ngfree++
	}
	unlock(&sched.gflock)
}

// Breakpoint executes a breakpoint trap.
func Breakpoint() {
	breakpoint()
}

// dolockOSThread is called by LockOSThread and lockOSThread below
// after they modify m.locked. Do not allow preemption during this call,
// or else the m might be different in this function than in the caller.
//go:nosplit
// 将当前的G和M进行互相绑定
// M的lockedg保存G
// G的lockedm保存M
func dolockOSThread() {
	_g_ := getg()
	_g_.m.lockedg.set(_g_)
	_g_.lockedm.set(_g_.m)
}

//go:nosplit

// LockOSThread wires the calling goroutine to its current operating system thread.
// The calling goroutine will always execute in that thread,
// and no other goroutine will execute in it,
// until the calling goroutine has made as many calls to
// UnlockOSThread as to LockOSThread.
// If the calling goroutine exits without unlocking the thread,
// the thread will be terminated.
//
// A goroutine should call LockOSThread before calling OS services or
// non-Go library functions that depend on per-thread state.
func LockOSThread() {
	if atomic.Load(&newmHandoff.haveTemplateThread) == 0 && GOOS != "plan9" {
		// If we need to start a new thread from the locked
		// thread, we need the template thread. Start it now
		// while we're in a known-good state.
		startTemplateThread()
	}
	_g_ := getg()
	_g_.m.lockedExt++
	if _g_.m.lockedExt == 0 {
		_g_.m.lockedExt--
		panic("LockOSThread nesting overflow")
	}
	dolockOSThread()
}

//go:nosplit
// lockOSThread 实现 g 和 m 的绑定
func lockOSThread() {
	// 设置 m.lockedInt 加1
	getg().m.lockedInt++
	dolockOSThread()
}

// dounlockOSThread is called by UnlockOSThread and unlockOSThread below
// after they update m->locked. Do not allow preemption during this call,
// or else the m might be in different in this function than in the caller.
//go:nosplit
func dounlockOSThread() {
	_g_ := getg()
	if _g_.m.lockedInt != 0 || _g_.m.lockedExt != 0 {
		return
	}
	_g_.m.lockedg = 0
	_g_.lockedm = 0
}

//go:nosplit

// UnlockOSThread undoes an earlier call to LockOSThread.
// If this drops the number of active LockOSThread calls on the
// calling goroutine to zero, it unwires the calling goroutine from
// its fixed operating system thread.
// If there are no active LockOSThread calls, this is a no-op.
//
// Before calling UnlockOSThread, the caller must ensure that the OS
// thread is suitable for running other goroutines. If the caller made
// any permanent changes to the state of the thread that would affect
// other goroutines, it should not call this function and thus leave
// the goroutine locked to the OS thread until the goroutine (and
// hence the thread) exits.
func UnlockOSThread() {
	_g_ := getg()
	if _g_.m.lockedExt == 0 {
		return
	}
	_g_.m.lockedExt--
	dounlockOSThread()
}

//go:nosplit
func unlockOSThread() {
	_g_ := getg()
	if _g_.m.lockedInt == 0 {
		systemstack(badunlockosthread)
	}
	_g_.m.lockedInt--
	dounlockOSThread()
}

func badunlockosthread() {
	throw("runtime: internal error: misuse of lockOSThread/unlockOSThread")
}

func gcount() int32 {
	n := int32(allglen) - sched.ngfree - int32(atomic.Load(&sched.ngsys))
	for _, _p_ := range allp {
		n -= _p_.gfreecnt
	}

	// All these variables can be changed concurrently, so the result can be inconsistent.
	// But at least the current goroutine is running.
	if n < 1 {
		n = 1
	}
	return n
}

func mcount() int32 {
	return int32(sched.mnext - sched.nmfreed)
}

var prof struct {
	signalLock uint32
	hz         int32
}

func _System()                    { _System() }
func _ExternalCode()              { _ExternalCode() }
func _LostExternalCode()          { _LostExternalCode() }
func _GC()                        { _GC() }
func _LostSIGPROFDuringAtomic64() { _LostSIGPROFDuringAtomic64() }

// Counts SIGPROFs received while in atomic64 critical section, on mips{,le}
var lostAtomic64Count uint64

// Called if we receive a SIGPROF signal.
// Called by the signal handler, may run during STW.
//go:nowritebarrierrec
func sigprof(pc, sp, lr uintptr, gp *g, mp *m) {
	if prof.hz == 0 {
		return
	}

	// On mips{,le}, 64bit atomics are emulated with spinlocks, in
	// runtime/internal/atomic. If SIGPROF arrives while the program is inside
	// the critical section, it creates a deadlock (when writing the sample).
	// As a workaround, create a counter of SIGPROFs while in critical section
	// to store the count, and pass it to sigprof.add() later when SIGPROF is
	// received from somewhere else (with _LostSIGPROFDuringAtomic64 as pc).
	if GOARCH == "mips" || GOARCH == "mipsle" {
		if f := findfunc(pc); f.valid() {
			if hasprefix(funcname(f), "runtime/internal/atomic") {
				lostAtomic64Count++
				return
			}
		}
	}

	// Profiling runs concurrently with GC, so it must not allocate.
	// Set a trap in case the code does allocate.
	// Note that on windows, one thread takes profiles of all the
	// other threads, so mp is usually not getg().m.
	// In fact mp may not even be stopped.
	// See golang.org/issue/17165.
	getg().m.mallocing++

	// Define that a "user g" is a user-created goroutine, and a "system g"
	// is one that is m->g0 or m->gsignal.
	//
	// We might be interrupted for profiling halfway through a
	// goroutine switch. The switch involves updating three (or four) values:
	// g, PC, SP, and (on arm) LR. The PC must be the last to be updated,
	// because once it gets updated the new g is running.
	//
	// When switching from a user g to a system g, LR is not considered live,
	// so the update only affects g, SP, and PC. Since PC must be last, there
	// the possible partial transitions in ordinary execution are (1) g alone is updated,
	// (2) both g and SP are updated, and (3) SP alone is updated.
	// If SP or g alone is updated, we can detect the partial transition by checking
	// whether the SP is within g's stack bounds. (We could also require that SP
	// be changed only after g, but the stack bounds check is needed by other
	// cases, so there is no need to impose an additional requirement.)
	//
	// There is one exceptional transition to a system g, not in ordinary execution.
	// When a signal arrives, the operating system starts the signal handler running
	// with an updated PC and SP. The g is updated last, at the beginning of the
	// handler. There are two reasons this is okay. First, until g is updated the
	// g and SP do not match, so the stack bounds check detects the partial transition.
	// Second, signal handlers currently run with signals disabled, so a profiling
	// signal cannot arrive during the handler.
	//
	// When switching from a system g to a user g, there are three possibilities.
	//
	// First, it may be that the g switch has no PC update, because the SP
	// either corresponds to a user g throughout (as in asmcgocall)
	// or because it has been arranged to look like a user g frame
	// (as in cgocallback_gofunc). In this case, since the entire
	// transition is a g+SP update, a partial transition updating just one of
	// those will be detected by the stack bounds check.
	//
	// Second, when returning from a signal handler, the PC and SP updates
	// are performed by the operating system in an atomic update, so the g
	// update must be done before them. The stack bounds check detects
	// the partial transition here, and (again) signal handlers run with signals
	// disabled, so a profiling signal cannot arrive then anyway.
	//
	// Third, the common case: it may be that the switch updates g, SP, and PC
	// separately. If the PC is within any of the functions that does this,
	// we don't ask for a traceback. C.F. the function setsSP for more about this.
	//
	// There is another apparently viable approach, recorded here in case
	// the "PC within setsSP function" check turns out not to be usable.
	// It would be possible to delay the update of either g or SP until immediately
	// before the PC update instruction. Then, because of the stack bounds check,
	// the only problematic interrupt point is just before that PC update instruction,
	// and the sigprof handler can detect that instruction and simulate stepping past
	// it in order to reach a consistent state. On ARM, the update of g must be made
	// in two places (in R10 and also in a TLS slot), so the delayed update would
	// need to be the SP update. The sigprof handler must read the instruction at
	// the current PC and if it was the known instruction (for example, JMP BX or
	// MOV R2, PC), use that other register in place of the PC value.
	// The biggest drawback to this solution is that it requires that we can tell
	// whether it's safe to read from the memory pointed at by PC.
	// In a correct program, we can test PC == nil and otherwise read,
	// but if a profiling signal happens at the instant that a program executes
	// a bad jump (before the program manages to handle the resulting fault)
	// the profiling handler could fault trying to read nonexistent memory.
	//
	// To recap, there are no constraints on the assembly being used for the
	// transition. We simply require that g and SP match and that the PC is not
	// in gogo.
	traceback := true
	if gp == nil || sp < gp.stack.lo || gp.stack.hi < sp || setsSP(pc) {
		traceback = false
	}
	var stk [maxCPUProfStack]uintptr
	n := 0
	if mp.ncgo > 0 && mp.curg != nil && mp.curg.syscallpc != 0 && mp.curg.syscallsp != 0 {
		cgoOff := 0
		// Check cgoCallersUse to make sure that we are not
		// interrupting other code that is fiddling with
		// cgoCallers.  We are running in a signal handler
		// with all signals blocked, so we don't have to worry
		// about any other code interrupting us.
		if atomic.Load(&mp.cgoCallersUse) == 0 && mp.cgoCallers != nil && mp.cgoCallers[0] != 0 {
			for cgoOff < len(mp.cgoCallers) && mp.cgoCallers[cgoOff] != 0 {
				cgoOff++
			}
			copy(stk[:], mp.cgoCallers[:cgoOff])
			mp.cgoCallers[0] = 0
		}

		// Collect Go stack that leads to the cgo call.
		n = gentraceback(mp.curg.syscallpc, mp.curg.syscallsp, 0, mp.curg, 0, &stk[cgoOff], len(stk)-cgoOff, nil, nil, 0)
	} else if traceback {
		n = gentraceback(pc, sp, lr, gp, 0, &stk[0], len(stk), nil, nil, _TraceTrap|_TraceJumpStack)
	}

	if n <= 0 {
		// Normal traceback is impossible or has failed.
		// See if it falls into several common cases.
		n = 0
		if GOOS == "windows" && mp.libcallg != 0 && mp.libcallpc != 0 && mp.libcallsp != 0 {
			// Libcall, i.e. runtime syscall on windows.
			// Collect Go stack that leads to the call.
			n = gentraceback(mp.libcallpc, mp.libcallsp, 0, mp.libcallg.ptr(), 0, &stk[0], len(stk), nil, nil, 0)
		}
		if n == 0 {
			// If all of the above has failed, account it against abstract "System" or "GC".
			n = 2
			// "ExternalCode" is better than "etext".
			if pc > firstmoduledata.etext {
				pc = funcPC(_ExternalCode) + sys.PCQuantum
			}
			stk[0] = pc
			if mp.preemptoff != "" || mp.helpgc != 0 {
				stk[1] = funcPC(_GC) + sys.PCQuantum
			} else {
				stk[1] = funcPC(_System) + sys.PCQuantum
			}
		}
	}

	if prof.hz != 0 {
		if (GOARCH == "mips" || GOARCH == "mipsle") && lostAtomic64Count > 0 {
			cpuprof.addLostAtomic64(lostAtomic64Count)
			lostAtomic64Count = 0
		}
		cpuprof.add(gp, stk[:n])
	}
	getg().m.mallocing--
}

// If the signal handler receives a SIGPROF signal on a non-Go thread,
// it tries to collect a traceback into sigprofCallers.
// sigprofCallersUse is set to non-zero while sigprofCallers holds a traceback.
var sigprofCallers cgoCallers
var sigprofCallersUse uint32

// sigprofNonGo is called if we receive a SIGPROF signal on a non-Go thread,
// and the signal handler collected a stack trace in sigprofCallers.
// When this is called, sigprofCallersUse will be non-zero.
// g is nil, and what we can do is very limited.
//go:nosplit
//go:nowritebarrierrec
func sigprofNonGo() {
	if prof.hz != 0 {
		n := 0
		for n < len(sigprofCallers) && sigprofCallers[n] != 0 {
			n++
		}
		cpuprof.addNonGo(sigprofCallers[:n])
	}

	atomic.Store(&sigprofCallersUse, 0)
}

// sigprofNonGoPC is called when a profiling signal arrived on a
// non-Go thread and we have a single PC value, not a stack trace.
// g is nil, and what we can do is very limited.
//go:nosplit
//go:nowritebarrierrec
func sigprofNonGoPC(pc uintptr) {
	if prof.hz != 0 {
		stk := []uintptr{
			pc,
			funcPC(_ExternalCode) + sys.PCQuantum,
		}
		cpuprof.addNonGo(stk)
	}
}

// Reports whether a function will set the SP
// to an absolute value. Important that
// we don't traceback when these are at the bottom
// of the stack since we can't be sure that we will
// find the caller.
//
// If the function is not on the bottom of the stack
// we assume that it will have set it up so that traceback will be consistent,
// either by being a traceback terminating function
// or putting one on the stack at the right offset.
func setsSP(pc uintptr) bool {
	f := findfunc(pc)
	if !f.valid() {
		// couldn't find the function for this PC,
		// so assume the worst and stop traceback
		return true
	}
	switch f.funcID {
	case funcID_gogo, funcID_systemstack, funcID_mcall, funcID_morestack:
		return true
	}
	return false
}

// setcpuprofilerate sets the CPU profiling rate to hz times per second.
// If hz <= 0, setcpuprofilerate turns off CPU profiling.
func setcpuprofilerate(hz int32) {
	// Force sane arguments.
	if hz < 0 {
		hz = 0
	}

	// Disable preemption, otherwise we can be rescheduled to another thread
	// that has profiling enabled.
	_g_ := getg()
	_g_.m.locks++

	// Stop profiler on this thread so that it is safe to lock prof.
	// if a profiling signal came in while we had prof locked,
	// it would deadlock.
	setThreadCPUProfiler(0)

	for !atomic.Cas(&prof.signalLock, 0, 1) {
		osyield()
	}
	if prof.hz != hz {
		setProcessCPUProfiler(hz)
		prof.hz = hz
	}
	atomic.Store(&prof.signalLock, 0)

	lock(&sched.lock)
	sched.profilehz = hz
	unlock(&sched.lock)

	if hz != 0 {
		setThreadCPUProfiler(hz)
	}

	_g_.m.locks--
}

// Change number of processors. The world is stopped, sched is locked.
// gcworkbufs are not being modified by either the GC or
// the write barrier code.
// Returns list of Ps with local work, they need to be scheduled by the caller.
// 所有的P都在这个函数分配，不管是最开始的初始化分配，还是后期调整
func procresize(nprocs int32) *p {
	old := gomaxprocs
	// 如果 gomaxprocs <=0 抛出异常
	if old < 0 || nprocs <= 0 {
		throw("procresize: invalid arg")
	}
	// 如果开启trace，记录事件
	if trace.enabled {
		traceGomaxprocs(nprocs)
	}

	// update statistics
	// 更新全局状态统计
	now := nanotime()
	if sched.procresizetime != 0 {
		sched.totaltime += int64(old) * (now - sched.procresizetime)
	}
	sched.procresizetime = now

	// Grow allp if necessary.
	if nprocs > int32(len(allp)) {
		// Synchronize with retake, which could be running
		// concurrently since it doesn't run on a P.
		lock(&allpLock)
		if nprocs <= int32(cap(allp)) {
			allp = allp[:nprocs]
		} else {
			// 分配nprocs个*p
			nallp := make([]*p, nprocs)
			// Copy everything up to allp's cap so we
			// never lose old allocated Ps.
			copy(nallp, allp[:cap(allp)])
			allp = nallp
		}
		unlock(&allpLock)
	}

	// initialize new P's
	// 检查p是否初始化了，如果没有初始化，则进行初始化
	for i := int32(0); i < nprocs; i++ {
		pp := allp[i]
		// 如果p是nil，进行初始化
		if pp == nil {
			pp = new(p)
			pp.id = i
			pp.status = _Pgcstop            // 更改状态
			pp.sudogcache = pp.sudogbuf[:0] //将sudogcache指向sudogbuf的起始地址
			for i := range pp.deferpool {
				pp.deferpool[i] = pp.deferpoolbuf[i][:0]
			}
			pp.wbBuf.reset()
			// 将pp保存到allp数组里, allp[i] = pp
			atomicstorep(unsafe.Pointer(&allp[i]), unsafe.Pointer(pp))
		}
		if pp.mcache == nil {
			if old == 0 && i == 0 {
				if getg().m.mcache == nil {
					throw("missing mcache?")
				}
				pp.mcache = getg().m.mcache // bootstrap
			} else {
				pp.mcache = allocmcache()
			}
		}
		if raceenabled && pp.racectx == 0 {
			if old == 0 && i == 0 {
				pp.racectx = raceprocctx0
				raceprocctx0 = 0 // bootstrap
			} else {
				pp.racectx = raceproccreate()
			}
		}
	}

	// free unused P's
	for i := nprocs; i < old; i++ {
		p := allp[i]
		if trace.enabled && p == getg().m.p.ptr() {
			// moving to p[0], pretend that we were descheduled
			// and then scheduled again to keep the trace sane.
			traceGoSched()
			traceProcStop(p)
		}
		// move all runnable goroutines to the global queue
		for p.runqhead != p.runqtail {
			// pop from tail of local queue
			p.runqtail--
			gp := p.runq[p.runqtail%uint32(len(p.runq))].ptr()
			// push onto head of global queue
			globrunqputhead(gp)
		}
		if p.runnext != 0 {
			globrunqputhead(p.runnext.ptr())
			p.runnext = 0
		}
		// if there's a background worker, make it runnable and put
		// it on the global queue so it can clean itself up
		if gp := p.gcBgMarkWorker.ptr(); gp != nil {
			casgstatus(gp, _Gwaiting, _Grunnable)
			if trace.enabled {
				traceGoUnpark(gp, 0)
			}
			globrunqput(gp)
			// This assignment doesn't race because the
			// world is stopped.
			p.gcBgMarkWorker.set(nil)
		}
		// Flush p's write barrier buffer.
		if gcphase != _GCoff {
			wbBufFlush1(p)
			p.gcw.dispose()
		}
		for i := range p.sudogbuf {
			p.sudogbuf[i] = nil
		}
		p.sudogcache = p.sudogbuf[:0]
		for i := range p.deferpool {
			for j := range p.deferpoolbuf[i] {
				p.deferpoolbuf[i][j] = nil
			}
			p.deferpool[i] = p.deferpoolbuf[i][:0]
		}
		freemcache(p.mcache)
		p.mcache = nil
		gfpurge(p)
		traceProcFree(p)
		if raceenabled {
			raceprocdestroy(p.racectx)
			p.racectx = 0
		}
		p.gcAssistTime = 0
		p.status = _Pdead
		// can't free P itself because it can be referenced by an M in syscall
	}

	// Trim allp.
	if int32(len(allp)) != nprocs {
		lock(&allpLock)
		allp = allp[:nprocs]
		unlock(&allpLock)
	}

	_g_ := getg()
	// 如果当前的M已经绑定P，继续使用，否则将当前的M绑定一个P
	if _g_.m.p != 0 && _g_.m.p.ptr().id < nprocs {
		// continue to use the current P
		_g_.m.p.ptr().status = _Prunning
	} else {
		// release the current P and acquire allp[0]
		// 获取allp[0]
		if _g_.m.p != 0 {
			_g_.m.p.ptr().m = 0
		}
		_g_.m.p = 0
		_g_.m.mcache = nil
		p := allp[0]
		p.m = 0
		p.status = _Pidle
		// 将当前的m和p绑定
		acquirep(p)
		if trace.enabled {
			traceGoStart()
		}
	}
	var runnablePs *p
	for i := nprocs - 1; i >= 0; i-- {
		p := allp[i]
		// 如果是当前的M绑定的P，不放入P空闲链表
		// 否则更改P的状态为_Pidle，放入P空闲链表
		if _g_.m.p.ptr() == p {
			continue
		}
		p.status = _Pidle
		if runqempty(p) { // 将空闲p放入空闲链表
			pidleput(p)
		} else {
			p.m.set(mget())
			// ? 为什么不先runnablePs = p，再p.link.set(runnablePs)，效果应该是一样的
			p.link.set(runnablePs)
			runnablePs = p
		}
	}
	stealOrder.reset(uint32(nprocs))
	var int32p *int32 = &gomaxprocs // make compiler check that gomaxprocs is an int32
	atomic.Store((*uint32)(unsafe.Pointer(int32p)), uint32(nprocs))
	return runnablePs
}

// Associate p and the current m.
//
// This function is allowed to have write barriers even if the caller
// isn't because it immediately acquires _p_.
//
//go:yeswritebarrierrec
// 将当前的M和p绑定
func acquirep(_p_ *p) {
	// Do the part that isn't allowed to have write barriers.
	acquirep1(_p_)

	// have p; write barriers now allowed
	_g_ := getg()
	_g_.m.mcache = _p_.mcache

	if trace.enabled {
		traceProcStart()
	}
}

// acquirep1 is the first step of acquirep, which actually acquires
// _p_. This is broken out so we can disallow write barriers for this
// part, since we don't yet have a P.
//
//go:nowritebarrierrec
func acquirep1(_p_ *p) {
	_g_ := getg()

	if _g_.m.p != 0 || _g_.m.mcache != nil {
		throw("acquirep: already in go")
	}
	if _p_.m != 0 || _p_.status != _Pidle {
		id := int64(0)
		if _p_.m != 0 {
			id = _p_.m.ptr().id
		}
		print("acquirep: p->m=", _p_.m, "(", id, ") p->status=", _p_.status, "\n")
		throw("acquirep: invalid p state")
	}
	_g_.m.p.set(_p_)
	_p_.m.set(_g_.m)
	_p_.status = _Prunning
}

// Disassociate p and the current m.
func releasep() *p {
	_g_ := getg()

	if _g_.m.p == 0 || _g_.m.mcache == nil {
		throw("releasep: invalid arg")
	}
	_p_ := _g_.m.p.ptr()
	if _p_.m.ptr() != _g_.m || _p_.mcache != _g_.m.mcache || _p_.status != _Prunning {
		print("releasep: m=", _g_.m, " m->p=", _g_.m.p.ptr(), " p->m=", _p_.m, " m->mcache=", _g_.m.mcache, " p->mcache=", _p_.mcache, " p->status=", _p_.status, "\n")
		throw("releasep: invalid p state")
	}
	if trace.enabled {
		traceProcStop(_g_.m.p.ptr())
	}
	_g_.m.p = 0
	_g_.m.mcache = nil
	_p_.m = 0
	_p_.status = _Pidle
	return _p_
}

func incidlelocked(v int32) {
	lock(&sched.lock)
	sched.nmidlelocked += v
	if v > 0 {
		checkdead()
	}
	unlock(&sched.lock)
}

// Check for deadlock situation.
// The check is based on number of running M's, if 0 -> deadlock.
// sched.lock must be held.
// 检查死锁情况。
// 检查基于正在运行的M的数量，如果是0表示死锁。
// 必须持有sched.lock。
func checkdead() {
	// For -buildmode=c-shared or -buildmode=c-archive it's OK if
	// there are no running goroutines. The calling program is
	// assumed to be running.
	// 如果是编译成c的库，直接返回
	if islibrary || isarchive {
		return
	}

	// If we are dying because of a signal caught on an already idle thread,
	// freezetheworld will cause all running threads to block.
	// And runtime will essentially enter into deadlock state,
	// except that there is a thread that will call exit soon.
	if panicking > 0 {
		return
	}

	// m个数-没事做的m-被锁的m-runtime自身使用的m（如：sysmon）
	// 也就是正在运行的m的个数
	run := mcount() - sched.nmidle - sched.nmidlelocked - sched.nmsys
	if run > 0 {
		return
	}
	if run < 0 {
		print("runtime: checkdead: nmidle=", sched.nmidle, " nmidlelocked=", sched.nmidlelocked, " mcount=", mcount(), " nmsys=", sched.nmsys, "\n")
		throw("checkdead: inconsistent counts")
	}

	grunning := 0
	lock(&allglock)
	for i := 0; i < len(allgs); i++ {
		gp := allgs[i]
		if isSystemGoroutine(gp) {
			continue
		}
		s := readgstatus(gp)
		switch s &^ _Gscan {
		case _Gwaiting:
			grunning++
		case _Grunnable,
			_Grunning,
			_Gsyscall:
			unlock(&allglock)
			print("runtime: checkdead: find g ", gp.goid, " in status ", s, "\n")
			throw("checkdead: runnable g")
		}
	}
	unlock(&allglock)
	if grunning == 0 { // possible if main goroutine calls runtime·Goexit()
		throw("no goroutines (main called runtime.Goexit) - deadlock!")
	}

	// Maybe jump time forward for playground.
	gp := timejump()
	if gp != nil {
		casgstatus(gp, _Gwaiting, _Grunnable)
		globrunqput(gp)
		_p_ := pidleget()
		if _p_ == nil {
			throw("checkdead: no p for timer")
		}
		mp := mget()
		if mp == nil {
			// There should always be a free M since
			// nothing is running.
			throw("checkdead: no m for timer")
		}
		mp.nextp.set(_p_)
		notewakeup(&mp.park)
		return
	}

	getg().m.throwing = -1 // do not dump full stacks
	throw("all goroutines are asleep - deadlock!")
}

// forcegcperiod is the maximum time in nanoseconds between garbage
// collections. If we go this long without a garbage collection, one
// is forced to run.
//
// This is a variable for testing purposes. It normally doesn't change.
var forcegcperiod int64 = 2 * 60 * 1e9 // 2min

// Always runs without a P, so write barriers are not allowed.
//
//go:nowritebarrierrec
// 系统后台监控，而且这个函数不符合GPM模型，该函数直接占用一个M，且不需要P，没有任何上下文切换，用不着P
// sysmon中有netpool(获取fd事件), retake(抢占), forcegc(按时间强制执行gc),
// scavenge heap(释放自由列表中多余的项减少内存占用)等处理.
// cgo和syscall时，p的状态会被设置为_Psyscall，sysmon周期性地检查并retake p，
// 如果发现p处于这个状态且超过10ms就会强制性收回p，m从cgo和syscall返回后会重新尝试拿p，进入调度循环。
// 检测系统的运行情况，比如 checkdead()
func sysmon() {
	lock(&sched.lock)
	sched.nmsys++
	// 判断程序是否死锁
	checkdead()
	unlock(&sched.lock)

	// If a heap span goes unused for 5 minutes after a garbage collection,
	// we hand it back to the operating system.
	scavengelimit := int64(5 * 60 * 1e9)

	// 	scavenge: scavenge=1 enables debugging mode of heap scavenger.
	// 如果设置了scavenge=1，那么开启debugging
	if debug.scavenge > 0 {
		// Scavenge-a-lot for testing.
		forcegcperiod = 10 * 1e6
		scavengelimit = 20 * 1e6
	}

	lastscavenge := nanotime()
	nscavenge := 0

	lasttrace := int64(0)
	idle := 0 // how many cycles in succession we had not wokeup somebody
	delay := uint32(0)
	for {
		if idle == 0 { // start with 20us sleep...
			delay = 20
		} else if idle > 50 { // start doubling the sleep after 1ms...
			delay *= 2
		}
		if delay > 10*1000 { // up to 10ms
			delay = 10 * 1000
		}
		// 休眠delay us
		usleep(delay)
		if debug.schedtrace <= 0 && (sched.gcwaiting != 0 || atomic.Load(&sched.npidle) == uint32(gomaxprocs)) {
			lock(&sched.lock)
			if atomic.Load(&sched.gcwaiting) != 0 || atomic.Load(&sched.npidle) == uint32(gomaxprocs) {
				atomic.Store(&sched.sysmonwait, 1)
				unlock(&sched.lock)
				// Make wake-up period small enough
				// for the sampling to be correct.
				maxsleep := forcegcperiod / 2
				if scavengelimit < forcegcperiod {
					maxsleep = scavengelimit / 2
				}
				shouldRelax := true
				if osRelaxMinNS > 0 {
					next := timeSleepUntil()
					now := nanotime()
					if next-now < osRelaxMinNS {
						shouldRelax = false
					}
				}
				if shouldRelax {
					osRelax(true)
				}
				notetsleep(&sched.sysmonnote, maxsleep)
				if shouldRelax {
					osRelax(false)
				}
				lock(&sched.lock)
				atomic.Store(&sched.sysmonwait, 0)
				noteclear(&sched.sysmonnote)
				idle = 0
				delay = 20
			}
			unlock(&sched.lock)
		}
		// trigger libc interceptors if needed
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		// poll network if not polled for more than 10ms
		lastpoll := int64(atomic.Load64(&sched.lastpoll))
		now := nanotime()
		// 如果超过10ms都没进行 netpoll ，那么强制执行一次 netpoll，
		// 并且如果获取到了可运行的G，那么插入全局列表。
		if netpollinited() && lastpoll != 0 && lastpoll+10*1000*1000 < now {
			atomic.Cas64(&sched.lastpoll, uint64(lastpoll), uint64(now))
			gp := netpoll(false) // non-blocking - returns list of goroutines
			if gp != nil {
				// Need to decrement number of idle locked M's
				// (pretending that one more is running) before injectglist.
				// Otherwise it can lead to the following situation:
				// injectglist grabs all P's but before it starts M's to run the P's,
				// another M returns from syscall, finishes running its G,
				// observes that there is no work to do and no other running M's
				// and reports deadlock.
				incidlelocked(-1)
				injectglist(gp)
				incidlelocked(1)
			}
		}
		// retake P's blocked in syscalls
		// and preempt long running G's
		// 抢夺阻塞时间长的syscall的G
		if retake(now) != 0 {
			idle = 0
		} else {
			idle++
		}

		// check if we need to force a GC
		// 检查是否超过2min未触发gc，如果是，那么强制触发gc
		if t := (gcTrigger{kind: gcTriggerTime, now: now}); t.test() && atomic.Load(&forcegc.idle) != 0 {
			lock(&forcegc.lock)
			forcegc.idle = 0
			forcegc.g.schedlink = 0
			// forcegc.g = forcegchelper
			injectglist(forcegc.g)
			unlock(&forcegc.lock)
		}

		// scavenge heap once in a while
		if lastscavenge+scavengelimit/2 < now {
			mheap_.scavenge(int32(nscavenge), uint64(now), uint64(scavengelimit))
			lastscavenge = now
			nscavenge++
		}
		if debug.schedtrace > 0 && lasttrace+int64(debug.schedtrace)*1000000 <= now {
			lasttrace = now
			// scheddetail: setting schedtrace=X and scheddetail=1 causes the scheduler to emit
			schedtrace(debug.scheddetail > 0)
		}
	}
}

type sysmontick struct {
	schedtick   uint32
	schedwhen   int64
	syscalltick uint32
	syscallwhen int64
}

// forcePreemptNS is the time slice given to a G before it is
// preempted.
const forcePreemptNS = 10 * 1000 * 1000 // 10ms

// 实现go调度系统的抢占
// retake()函数会遍历所有的P，如果一个P处于执行状态，
// 且已经连续执行了较长时间，就会被抢占。
// retake()调用preemptone()将P的stackguard0设为
// stackPreempt(关于stackguard的详细内容，可以参考 Split Stacks)，
// 这将导致该P中正在执行的G进行下一次函数调用时，
// 导致栈空间检查失败。进而触发morestack()（汇编代码，位于asm_XXX.s中）
// 然后进行一连串的函数调用，主要的调用过程如下：
// morestack()（汇编代码）-> newstack() -> gopreempt_m() -> goschedImpl() -> schedule()
// http://ga0.github.io/golang/2015/09/20/golang-runtime-scheduler.html
func retake(now int64) uint32 {
	n := 0
	// Prevent allp slice changes. This lock will be completely
	// uncontended unless we're already stopping the world.
	lock(&allpLock)
	// We can't use a range loop over allp because we may
	// temporarily drop the allpLock. Hence, we need to re-fetch
	// allp each time around the loop.
	for i := 0; i < len(allp); i++ {
		_p_ := allp[i]
		if _p_ == nil {
			// This can happen if procresize has grown
			// allp but not yet created new Ps.
			continue
		}
		pd := &_p_.sysmontick
		s := _p_.status
		if s == _Psyscall {
			// Retake P from syscall if it's there for more than 1 sysmon tick (at least 20us).
			t := int64(_p_.syscalltick)
			if int64(pd.syscalltick) != t {
				pd.syscalltick = uint32(t)
				pd.syscallwhen = now
				continue
			}
			// On the one hand we don't want to retake Ps if there is no other work to do,
			// but on the other hand we want to retake them eventually
			// because they can prevent the sysmon thread from deep sleep.
			if runqempty(_p_) && atomic.Load(&sched.nmspinning)+atomic.Load(&sched.npidle) > 0 && pd.syscallwhen+10*1000*1000 > now {
				continue
			}
			// Drop allpLock so we can take sched.lock.
			unlock(&allpLock)
			// Need to decrement number of idle locked M's
			// (pretending that one more is running) before the CAS.
			// Otherwise the M from which we retake can exit the syscall,
			// increment nmidle and report deadlock.
			incidlelocked(-1)
			if atomic.Cas(&_p_.status, s, _Pidle) {
				if trace.enabled {
					traceGoSysBlock(_p_)
					traceProcStop(_p_)
				}
				n++
				_p_.syscalltick++
				handoffp(_p_)
			}
			incidlelocked(1)
			lock(&allpLock)
		} else if s == _Prunning {
			// Preempt G if it's running for too long.
			t := int64(_p_.schedtick)
			if int64(pd.schedtick) != t {
				pd.schedtick = uint32(t)
				pd.schedwhen = now
				continue
			}
			if pd.schedwhen+forcePreemptNS > now {
				continue
			}
			preemptone(_p_)
		}
	}
	unlock(&allpLock)
	return uint32(n)
}

// Tell all goroutines that they have been preempted and they should stop.
// This function is purely best-effort. It can fail to inform a goroutine if a
// processor just started running it.
// No locks need to be held.
// Returns true if preemption request was issued to at least one goroutine.
func preemptall() bool {
	res := false
	for _, _p_ := range allp {
		if _p_.status != _Prunning {
			continue
		}
		if preemptone(_p_) {
			res = true
		}
	}
	return res
}

// Tell the goroutine running on processor P to stop.
// This function is purely best-effort. It can incorrectly fail to inform the
// goroutine. It can send inform the wrong goroutine. Even if it informs the
// correct goroutine, that goroutine might ignore the request if it is
// simultaneously executing newstack.
// No lock needs to be held.
// Returns true if preemption request was issued.
// The actual preemption will happen at some point in the future
// and will be indicated by the gp->status no longer being
// Grunning
// 告诉在处理器P上运行的goroutine停止。这个功能纯粹是尽力而为。
// 它可能会错误地通知goroutine。它可以发送错误的goroutine。
// 即使它通知正确的goroutine，如果它同时执行newstack，那goroutine可能会忽略该请求。
// 不需要锁定。如果发出抢占请求，则返回true。实际的抢占将在未来的某个时刻发生，
// 并且将由gp->状态表示不再是Grunning
func preemptone(_p_ *p) bool {
	mp := _p_.m.ptr()
	if mp == nil || mp == getg().m {
		return false
	}
	gp := mp.curg
	if gp == nil || gp == mp.g0 {
		return false
	}

	// 标记可抢占
	gp.preempt = true

	// Every call in a go routine checks for stack overflow by
	// comparing the current stack pointer to gp->stackguard0.
	// Setting gp->stackguard0 to StackPreempt folds
	// preemption into the normal stack overflow check.
	// gorotuine 中的每个调用都会通过将当前堆栈指针与 gp->stackguard0 进行比较来检查堆栈溢出。
	// 将 gp->stackguard0 设置为 stackPreempt 会将抢占折叠为正常的堆栈溢出检查。
	gp.stackguard0 = stackPreempt
	return true
}

var starttime int64

// debug日志输出函数
func schedtrace(detailed bool) {
	now := nanotime()
	if starttime == 0 {
		starttime = now
	}

	lock(&sched.lock)
	print("SCHED ", (now-starttime)/1e6, "ms: gomaxprocs=", gomaxprocs, " idleprocs=", sched.npidle, " threads=", mcount(), " spinningthreads=", sched.nmspinning, " idlethreads=", sched.nmidle, " runqueue=", sched.runqsize)
	if detailed {
		print(" gcwaiting=", sched.gcwaiting, " nmidlelocked=", sched.nmidlelocked, " stopwait=", sched.stopwait, " sysmonwait=", sched.sysmonwait, "\n")
	}
	// We must be careful while reading data from P's, M's and G's.
	// Even if we hold schedlock, most data can be changed concurrently.
	// E.g. (p->m ? p->m->id : -1) can crash if p->m changes from non-nil to nil.
	for i, _p_ := range allp {
		mp := _p_.m.ptr()
		h := atomic.Load(&_p_.runqhead)
		t := atomic.Load(&_p_.runqtail)
		if detailed {
			id := int64(-1)
			if mp != nil {
				id = mp.id
			}
			print("  P", i, ": status=", _p_.status, " schedtick=", _p_.schedtick, " syscalltick=", _p_.syscalltick, " m=", id, " runqsize=", t-h, " gfreecnt=", _p_.gfreecnt, "\n")
		} else {
			// In non-detailed mode format lengths of per-P run queues as:
			// [len1 len2 len3 len4]
			print(" ")
			if i == 0 {
				print("[")
			}
			print(t - h)
			if i == len(allp)-1 {
				print("]\n")
			}
		}
	}

	if !detailed {
		unlock(&sched.lock)
		return
	}

	for mp := allm; mp != nil; mp = mp.alllink {
		_p_ := mp.p.ptr()
		gp := mp.curg
		lockedg := mp.lockedg.ptr()
		id1 := int32(-1)
		if _p_ != nil {
			id1 = _p_.id
		}
		id2 := int64(-1)
		if gp != nil {
			id2 = gp.goid
		}
		id3 := int64(-1)
		if lockedg != nil {
			id3 = lockedg.goid
		}
		print("  M", mp.id, ": p=", id1, " curg=", id2, " mallocing=", mp.mallocing, " throwing=", mp.throwing, " preemptoff=", mp.preemptoff, ""+" locks=", mp.locks, " dying=", mp.dying, " helpgc=", mp.helpgc, " spinning=", mp.spinning, " blocked=", mp.blocked, " lockedg=", id3, "\n")
	}

	lock(&allglock)
	for gi := 0; gi < len(allgs); gi++ {
		gp := allgs[gi]
		mp := gp.m
		lockedm := gp.lockedm.ptr()
		id1 := int64(-1)
		if mp != nil {
			id1 = mp.id
		}
		id2 := int64(-1)
		if lockedm != nil {
			id2 = lockedm.id
		}
		print("  G", gp.goid, ": status=", readgstatus(gp), "(", gp.waitreason, ") m=", id1, " lockedm=", id2, "\n")
	}
	unlock(&allglock)
	unlock(&sched.lock)
}

// Put mp on midle list.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
// 把mp添加到midle列表
func mput(mp *m) {
	mp.schedlink = sched.midle
	sched.midle.set(mp)
	sched.nmidle++
	checkdead()
}

// Try to get an m from midle list.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
func mget() *m {
	mp := sched.midle.ptr()
	if mp != nil {
		sched.midle = mp.schedlink
		sched.nmidle--
	}
	return mp
}

// Put gp on the global runnable queue.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
func globrunqput(gp *g) {
	gp.schedlink = 0
	if sched.runqtail != 0 {
		sched.runqtail.ptr().schedlink.set(gp)
	} else {
		sched.runqhead.set(gp)
	}
	sched.runqtail.set(gp)
	sched.runqsize++
}

// Put gp at the head of the global runnable queue.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
func globrunqputhead(gp *g) {
	gp.schedlink = sched.runqhead
	sched.runqhead.set(gp)
	if sched.runqtail == 0 {
		sched.runqtail.set(gp)
	}
	sched.runqsize++
}

// Put a batch of runnable goroutines on the global runnable queue.
// Sched must be locked.
func globrunqputbatch(ghead *g, gtail *g, n int32) {
	gtail.schedlink = 0
	if sched.runqtail != 0 {
		sched.runqtail.ptr().schedlink.set(ghead)
	} else {
		sched.runqhead.set(ghead)
	}
	sched.runqtail.set(gtail)
	sched.runqsize += n
}

// Try get a batch of G's from the global runnable queue.
// Sched must be locked.
// 由findrunnable调用，尝试从全局队列中取出一个G
// 并且转移一批G到P的本地队列，这样可以减少对全局队列的操作
// 也就减少了全局队列锁的操作
func globrunqget(_p_ *p, max int32) *g {
	// 如果全局队列的长度为0，直接返回
	if sched.runqsize == 0 {
		return nil
	}

	// 将全局队列任务等分， n=（G的个数）/（P的个数）+ 1
	n := sched.runqsize/gomaxprocs + 1
	if n > sched.runqsize {
		n = sched.runqsize
	}
	// 如果指定了max（max大于0表示指定了），n=max
	if max > 0 && n > max {
		n = max
	}
	// n不能超过p runq的一半，也就是不能超过128
	if n > int32(len(_p_.runq))/2 {
		n = int32(len(_p_.runq)) / 2
	}

	// 调整sched.runqsize的大小
	sched.runqsize -= n
	if sched.runqsize == 0 {
		sched.runqtail = 0
	}

	// 获取全局队列的头部的G，并在后面返回
	gp := sched.runqhead.ptr()
	sched.runqhead = gp.schedlink
	n--
	// 批量将n-1个全局队列中的G，放到P中
	for ; n > 0; n-- {
		gp1 := sched.runqhead.ptr()
		sched.runqhead = gp1.schedlink
		runqput(_p_, gp1, false)
	}
	return gp
}

// Put p to on _Pidle list.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
// 将P放入空闲P列表，并将sched.npidle加1
/*
	+-------+           +-------+
	|   p   |<-----+    |   p   |<------ pidleList
	+-------+      |    +-------+
	| link  |      +----| link  |
	+-------+           +-------+
*/
func pidleput(_p_ *p) {
	if !runqempty(_p_) {
		throw("pidleput: P has non-empty run queue")
	}
	_p_.link = sched.pidle
	sched.pidle.set(_p_)
	// 将sched.npidle加1
	atomic.Xadd(&sched.npidle, 1) // TODO: fast atomic
}

// Try get a p from _Pidle list.
// Sched must be locked.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
// 从空闲P列表获取一个P，并将sched.npidle减1
func pidleget() *p {
	_p_ := sched.pidle.ptr()
	if _p_ != nil {
		sched.pidle = _p_.link
		atomic.Xadd(&sched.npidle, -1) // TODO: fast atomic
	}
	return _p_
}

// runqempty returns true if _p_ has no Gs on its local run queue.
// It never returns true spuriously.
func runqempty(_p_ *p) bool {
	// Defend against a race where 1) _p_ has G1 in runqnext but runqhead == runqtail,
	// 2) runqput on _p_ kicks G1 to the runq, 3) runqget on _p_ empties runqnext.
	// Simply observing that runqhead == runqtail and then observing that runqnext == nil
	// does not mean the queue is empty.
	for {
		head := atomic.Load(&_p_.runqhead)
		tail := atomic.Load(&_p_.runqtail)
		runnext := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&_p_.runnext)))
		if tail == atomic.Load(&_p_.runqtail) {
			return head == tail && runnext == 0
		}
	}
}

// To shake out latent assumptions about scheduling order,
// we introduce some randomness into scheduling decisions
// when running with the race detector.
// The need for this was made obvious by changing the
// (deterministic) scheduling order in Go 1.5 and breaking
// many poorly-written tests.
// With the randomness here, as long as the tests pass
// consistently with -race, they shouldn't have latent scheduling
// assumptions.
const randomizeScheduler = raceenabled

// runqput tries to put g on the local runnable queue.
// If next is false, runqput adds g to the tail of the runnable queue.
// If next is true, runqput puts g in the _p_.runnext slot.
// If the run queue is full, runnext puts g on the global queue.
// Executed only by the owner P.
// 尝试将G放到P的本地队列
// 如果next==true，将G直接赋值给runnext
func runqput(_p_ *p, gp *g, next bool) {
	if randomizeScheduler && next && fastrand()%2 == 0 {
		next = false
	}

	if next {
	retryNext:
		// ? 为何让newg优先运行？
		// https://go-review.googlesource.com/c/go/+/9289
		oldnext := _p_.runnext
		// 将G赋值给_p_.runnext
		// 最新的G优先级最高，最可能先被执行。
		// 剩下的G如果go运行时调度器发现有空闲的core，就会把任务偷走点，
		// 让别的core执行，这样才能充分利用多核，提高并发能
		if !_p_.runnext.cas(oldnext, guintptr(unsafe.Pointer(gp))) {
			goto retryNext
		}
		if oldnext == 0 {
			return
		}
		// Kick the old runnext out to the regular run queue.
		gp = oldnext.ptr()
	}

retry:
	h := atomic.Load(&_p_.runqhead) // load-acquire, synchronize with consumers
	t := _p_.runqtail
	// 如果本地队列还有剩余的位置，将G插入本地队列的尾部
	if t-h < uint32(len(_p_.runq)) {
		_p_.runq[t%uint32(len(_p_.runq))].set(gp)
		atomic.Store(&_p_.runqtail, t+1) // store-release, makes the item available for consumption
		return
	}
	// 本地队列已满，放入全局队列
	if runqputslow(_p_, gp, h, t) {
		return
	}
	// the queue is not full, now the put above must succeed
	goto retry
}

// Put g and a batch of work from local runnable queue on global queue.
// Executed only by the owner P.
// 如果本地满了以后，一次将本地的一半的G转移到全局队列
func runqputslow(_p_ *p, gp *g, h, t uint32) bool {
	var batch [len(_p_.runq)/2 + 1]*g

	// First, grab a batch from local queue.
	n := t - h
	n = n / 2
	if n != uint32(len(_p_.runq)/2) {
		throw("runqputslow: queue is not full")
	}
	for i := uint32(0); i < n; i++ {
		batch[i] = _p_.runq[(h+i)%uint32(len(_p_.runq))].ptr()
	}
	if !atomic.Cas(&_p_.runqhead, h, h+n) { // cas-release, commits consume
		return false
	}
	batch[n] = gp

	if randomizeScheduler {
		for i := uint32(1); i <= n; i++ {
			j := fastrandn(i + 1)
			batch[i], batch[j] = batch[j], batch[i]
		}
	}

	// Link the goroutines.
	for i := uint32(0); i < n; i++ {
		batch[i].schedlink.set(batch[i+1])
	}

	// Now put the batch on global queue.
	lock(&sched.lock)
	// 将拿到的G，添加到全局队列末尾
	globrunqputbatch(batch[0], batch[n], int32(n+1))
	unlock(&sched.lock)
	return true
}

// Get g from local runnable queue.
// If inheritTime is true, gp should inherit the remaining time in the
// current time slice. Otherwise, it should start a new time slice.
// Executed only by the owner P.
// 从P的本地队列中获取G
// 优先获取p runnext，然后是本地列表
// 如果是是从runnext得到的G，那么inheritTime=true
func runqget(_p_ *p) (gp *g, inheritTime bool) {
	// If there's a runnext, it's the next G to run.
	for {
		next := _p_.runnext
		if next == 0 {
			break
		}
		if _p_.runnext.cas(next, 0) {
			return next.ptr(), true
		}
	}

	for {
		h := atomic.Load(&_p_.runqhead) // load-acquire, synchronize with other consumers
		t := _p_.runqtail
		if t == h {
			return nil, false
		}
		gp := _p_.runq[h%uint32(len(_p_.runq))].ptr()
		if atomic.Cas(&_p_.runqhead, h, h+1) { // cas-release, commits consume
			return gp, false
		}
	}
}

// Grabs a batch of goroutines from _p_'s runnable queue into batch.
// Batch is a ring buffer starting at batchHead.
// Returns number of grabbed goroutines.
// Can be executed by any P.
func runqgrab(_p_ *p, batch *[256]guintptr, batchHead uint32, stealRunNextG bool) uint32 {
	for {
		h := atomic.Load(&_p_.runqhead) // load-acquire, synchronize with other consumers
		t := atomic.Load(&_p_.runqtail) // load-acquire, synchronize with the producer
		n := t - h
		n = n - n/2
		if n == 0 {
			if stealRunNextG {
				// Try to steal from _p_.runnext.
				if next := _p_.runnext; next != 0 {
					if _p_.status == _Prunning {
						// Sleep to ensure that _p_ isn't about to run the g
						// we are about to steal.
						// The important use case here is when the g running
						// on _p_ ready()s another g and then almost
						// immediately blocks. Instead of stealing runnext
						// in this window, back off to give _p_ a chance to
						// schedule runnext. This will avoid thrashing gs
						// between different Ps.
						// A sync chan send/recv takes ~50ns as of time of
						// writing, so 3us gives ~50x overshoot.
						if GOOS != "windows" {
							usleep(3)
						} else {
							// On windows system timer granularity is
							// 1-15ms, which is way too much for this
							// optimization. So just yield.
							osyield()
						}
					}
					if !_p_.runnext.cas(next, 0) {
						continue
					}
					batch[batchHead%uint32(len(batch))] = next
					return 1
				}
			}
			return 0
		}
		if n > uint32(len(_p_.runq)/2) { // read inconsistent h and t
			continue
		}
		for i := uint32(0); i < n; i++ {
			g := _p_.runq[(h+i)%uint32(len(_p_.runq))]
			batch[(batchHead+i)%uint32(len(batch))] = g
		}
		if atomic.Cas(&_p_.runqhead, h, h+n) { // cas-release, commits consume
			return n
		}
	}
}

// Steal half of elements from local runnable queue of p2
// and put onto local runnable queue of p.
// Returns one of the stolen elements (or nil if failed).
// 从p2哪里偷一半的G放到p上
func runqsteal(_p_, p2 *p, stealRunNextG bool) *g {
	t := _p_.runqtail
	n := runqgrab(p2, &_p_.runq, t, stealRunNextG)
	if n == 0 {
		return nil
	}
	n--
	gp := _p_.runq[(t+n)%uint32(len(_p_.runq))].ptr()
	if n == 0 {
		return gp
	}
	h := atomic.Load(&_p_.runqhead) // load-acquire, synchronize with consumers
	if t-h+n >= uint32(len(_p_.runq)) {
		throw("runqsteal: runq overflow")
	}
	atomic.Store(&_p_.runqtail, t+n) // store-release, makes the item available for consumption
	return gp
}

//go:linkname setMaxThreads runtime/debug.setMaxThreads
// 设置最大的os线程数，超过的话直接panic
func setMaxThreads(in int) (out int) {
	lock(&sched.lock)
	out = int(sched.maxmcount)
	if in > 0x7fffffff { // MaxInt32
		sched.maxmcount = 0x7fffffff
	} else {
		sched.maxmcount = int32(in)
	}
	checkmcount()
	unlock(&sched.lock)
	return
}

func haveexperiment(name string) bool {
	if name == "framepointer" {
		return framepointer_enabled // set by linker
	}
	x := sys.Goexperiment
	for x != "" {
		xname := ""
		i := index(x, ",")
		if i < 0 {
			xname, x = x, ""
		} else {
			xname, x = x[:i], x[i+1:]
		}
		if xname == name {
			return true
		}
		if len(xname) > 2 && xname[:2] == "no" && xname[2:] == name {
			return false
		}
	}
	return false
}

//go:nosplit
func procPin() int {
	_g_ := getg()
	mp := _g_.m

	mp.locks++
	return int(mp.p.ptr().id)
}

//go:nosplit
func procUnpin() {
	_g_ := getg()
	_g_.m.locks--
}

//go:linkname sync_runtime_procPin sync.runtime_procPin
//go:nosplit
func sync_runtime_procPin() int {
	return procPin()
}

//go:linkname sync_runtime_procUnpin sync.runtime_procUnpin
//go:nosplit
func sync_runtime_procUnpin() {
	procUnpin()
}

//go:linkname sync_atomic_runtime_procPin sync/atomic.runtime_procPin
//go:nosplit
func sync_atomic_runtime_procPin() int {
	return procPin()
}

//go:linkname sync_atomic_runtime_procUnpin sync/atomic.runtime_procUnpin
//go:nosplit
func sync_atomic_runtime_procUnpin() {
	procUnpin()
}

// Active spinning for sync.Mutex.
//go:linkname sync_runtime_canSpin sync.runtime_canSpin
//go:nosplit
func sync_runtime_canSpin(i int) bool {
	// sync.Mutex is cooperative, so we are conservative with spinning.
	// Spin only few times and only if running on a multicore machine and
	// GOMAXPROCS>1 and there is at least one other running P and local runq is empty.
	// As opposed to runtime mutex we don't do passive spinning here,
	// because there can be work on global runq on on other Ps.
	if i >= active_spin || ncpu <= 1 || gomaxprocs <= int32(sched.npidle+sched.nmspinning)+1 {
		return false
	}
	if p := getg().m.p.ptr(); !runqempty(p) {
		return false
	}
	return true
}

//go:linkname sync_runtime_doSpin sync.runtime_doSpin
//go:nosplit
// 调用procyield函数，该函数是汇编语言实现。
// 函数内部循环调用PAUSE指令。PAUSE指令什么都不做，
// 但是会消耗CPU时间，在执行PAUSE指令时，
// CPU不会对他做不必要的优化
func sync_runtime_doSpin() {
	procyield(active_spin_cnt)
}

var stealOrder randomOrder

// randomOrder/randomEnum are helper types for randomized work stealing.
// They allow to enumerate all Ps in different pseudo-random orders without repetitions.
// The algorithm is based on the fact that if we have X such that X and GOMAXPROCS
// are coprime, then a sequences of (i + X) % GOMAXPROCS gives the required enumeration.
type randomOrder struct {
	count    uint32
	coprimes []uint32
}

type randomEnum struct {
	i     uint32
	count uint32
	pos   uint32
	inc   uint32
}

func (ord *randomOrder) reset(count uint32) {
	ord.count = count
	ord.coprimes = ord.coprimes[:0]
	for i := uint32(1); i <= count; i++ {
		if gcd(i, count) == 1 {
			ord.coprimes = append(ord.coprimes, i)
		}
	}
}

func (ord *randomOrder) start(i uint32) randomEnum {
	return randomEnum{
		count: ord.count,
		pos:   i % ord.count,
		inc:   ord.coprimes[i%uint32(len(ord.coprimes))],
	}
}

func (enum *randomEnum) done() bool {
	return enum.i == enum.count
}

func (enum *randomEnum) next() {
	enum.i++
	enum.pos = (enum.pos + enum.inc) % enum.count
}

func (enum *randomEnum) position() uint32 {
	return enum.pos
}

func gcd(a, b uint32) uint32 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
