package shell

import (
	"encoding/binary"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The page tunneld and the shim share, laid out as ttyshim.c's struct page
// asserts. Little-endian, as both architectures the shim is built for are.
const (
	pageSize    = 4096
	pageMagic   = 0x31595454 // "TTY1"
	pageVersion = 1

	offMagic   = 0
	offVersion = 4
	offLock    = 8
	offLoaded  = 12
	offDev     = 16
	offIno     = 24
	offIflag   = 32
	offOflag   = 36
	offCflag   = 40
	offLflag   = 44
	offLine    = 48
	offCC      = 49
	offIspeed  = 84
	offOspeed  = 88
	offRows    = 92
	offCols    = 94
	offFgPgrp  = 100
	offSid     = 104
	offGone    = 108
	offWant    = 112
	offDone    = 116
	offIflush  = 120
)

// lockPatience is how many tries the lock gets before its holder is asked
// after: the word holds the holder's pid, and only a holder that has died is
// taken over from. A live one is waited for, however long.
const lockPatience = 1000

// page is tunneld's mapping of one attach's shared page. Keys and timers can
// reach it after its run has ended, so every accessor holds mu for reading
// and answers as a fresh terminal once closed: touching the unmapped memory
// would fault and take tunneld down.
type page struct {
	path   string
	mem    []byte
	mu     sync.RWMutex
	closed bool
}

// newPage creates the page at path for the socket dev/ino, sized rows×cols,
// holding a fresh terminal's settings.
func newPage(path string, dev, ino uint64, rows, cols uint16) (*page, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(pageSize); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	mem, err := unix.Mmap(int(f.Fd()), 0, pageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	p := &page{path: path, mem: mem}
	le := binary.LittleEndian
	le.PutUint64(mem[offDev:], dev)
	le.PutUint64(mem[offIno:], ino)
	p.put(defaultMode)
	le.PutUint16(mem[offRows:], rows)
	le.PutUint16(mem[offCols:], cols)
	le.PutUint32(mem[offVersion:], pageVersion)
	atomic.StoreUint32(p.word(offMagic), pageMagic)
	return p, nil
}

func (p *page) word(off int) *uint32 { return (*uint32)(unsafe.Pointer(&p.mem[off])) }

func (p *page) lock() {
	me := uint32(os.Getpid())
	word := p.word(offLock)
	for i := 0; !atomic.CompareAndSwapUint32(word, 0, me); i++ {
		if i < lockPatience {
			runtime.Gosched()
			continue
		}
		if holder := atomic.LoadUint32(word); holder != 0 && holder != me &&
			unix.Kill(int(holder), 0) == unix.ESRCH &&
			atomic.CompareAndSwapUint32(word, holder, me) {
			return
		}
		time.Sleep(100 * time.Microsecond)
	}
}

func (p *page) unlock() { atomic.CompareAndSwapUint32(p.word(offLock), uint32(os.Getpid()), 0) }

// want is the latest drain the shim has asked tunneld to acknowledge.
func (p *page) want() uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return 0
	}
	return atomic.LoadUint32(p.word(offWant))
}

// ack says every byte read before a read that began once want was g has
// been through output processing: what the shim's drain waits for before
// it changes that processing.
func (p *page) ack(g uint32) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.closed {
		atomic.StoreUint32(p.word(offDone), g)
	}
}

// flushes counts the input flushes (tcflush, TCSAFLUSH) the program has
// asked for; the line being edited goes with each.
func (p *page) flushes() uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return 0
	}
	return atomic.LoadUint32(p.word(offIflush))
}

// settings is the terminal's settings as the program last set them.
func (p *page) settings() settings {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return defaultMode
	}
	p.lock()
	defer p.unlock()
	le := binary.LittleEndian
	var s settings
	s.iflag = le.Uint32(p.mem[offIflag:])
	s.oflag = le.Uint32(p.mem[offOflag:])
	s.cflag = le.Uint32(p.mem[offCflag:])
	s.lflag = le.Uint32(p.mem[offLflag:])
	s.line = p.mem[offLine]
	copy(s.cc[:], p.mem[offCC:offCC+32])
	s.ispeed = le.Uint32(p.mem[offIspeed:])
	s.ospeed = le.Uint32(p.mem[offOspeed:])
	return s
}

// put writes s without the lock: only newPage calls it, before the page is
// shared.
func (p *page) put(s settings) {
	le := binary.LittleEndian
	le.PutUint32(p.mem[offIflag:], s.iflag)
	le.PutUint32(p.mem[offOflag:], s.oflag)
	le.PutUint32(p.mem[offCflag:], s.cflag)
	le.PutUint32(p.mem[offLflag:], s.lflag)
	p.mem[offLine] = s.line
	copy(p.mem[offCC:offCC+32], s.cc[:])
	le.PutUint32(p.mem[offIspeed:], s.ispeed)
	le.PutUint32(p.mem[offOspeed:], s.ospeed)
}

// setSize records the page's terminal size.
func (p *page) setSize(rows, cols uint16) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return
	}
	p.lock()
	defer p.unlock()
	binary.LittleEndian.PutUint16(p.mem[offRows:], rows)
	binary.LittleEndian.PutUint16(p.mem[offCols:], cols)
}

// foreground is the process group the program last made the foreground; 0
// before any did.
func (p *page) foreground() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return 0
	}
	return int(int32(atomic.LoadUint32(p.word(offFgPgrp))))
}

// seed names pid, the program tunneld started in a session of its own, as
// the session and its first foreground group.
func (p *page) seed(pid int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return
	}
	atomic.StoreUint32(p.word(offSid), uint32(pid))
	atomic.CompareAndSwapUint32(p.word(offFgPgrp), 0, uint32(pid))
}

// loaded is whether any process has loaded the shim on this page.
func (p *page) loaded() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return !p.closed && atomic.LoadUint32(p.word(offLoaded)) != 0
}

// Close unmaps the page and removes its file; a second Close does nothing.
func (p *page) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	// What outlives the run (a nohup'd job) takes the shim out of what it
	// starts from now on; see ttyshim.c's needs.
	atomic.StoreUint32(p.word(offGone), 1)
	err := unix.Munmap(p.mem)
	if rmErr := os.Remove(p.path); err == nil && !os.IsNotExist(rmErr) {
		err = rmErr
	}
	return err
}
