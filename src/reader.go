package fzf

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charlievieth/fastwalk"
	"github.com/junegunn/fzf/src/util"
)

// Reader reads from command or standard input
type Reader struct {
	pusher   func([]byte) bool
	eventBox *util.EventBox
	delimNil bool
	event    int32
	finChan  chan bool
	mutex    sync.Mutex
	exec     *exec.Cmd
	command  *string
	killed   bool
	wait     bool
}

// NewReader returns new Reader object
func NewReader(pusher func([]byte) bool, eventBox *util.EventBox, delimNil bool, wait bool) *Reader {
	return &Reader{pusher, eventBox, delimNil, int32(EvtReady), make(chan bool, 1), sync.Mutex{}, nil, nil, false, wait}
}

func (r *Reader) startEventPoller() {
	go func() {
		ptr := &r.event
		pollInterval := readerPollIntervalMin
		for {
			if atomic.CompareAndSwapInt32(ptr, int32(EvtReadNew), int32(EvtReady)) {
				r.eventBox.Set(EvtReadNew, (*string)(nil))
				pollInterval = readerPollIntervalMin
			} else if atomic.LoadInt32(ptr) == int32(EvtReadFin) {
				if r.wait {
					r.finChan <- true
				}
				return
			} else {
				pollInterval += readerPollIntervalStep
				if pollInterval > readerPollIntervalMax {
					pollInterval = readerPollIntervalMax
				}
			}
			time.Sleep(pollInterval)
		}
	}()
}

func (r *Reader) fin(success bool) {
	atomic.StoreInt32(&r.event, int32(EvtReadFin))
	if r.wait {
		<-r.finChan
	}

	r.mutex.Lock()
	ret := r.command
	if success || r.killed {
		ret = nil
	}
	r.mutex.Unlock()

	r.eventBox.Set(EvtReadFin, ret)
}

func (r *Reader) terminate() {
	r.mutex.Lock()
	r.killed = true
	if r.exec != nil && r.exec.Process != nil {
		util.KillCommand(r.exec)
	} else {
		os.Stdin.Close()
	}
	r.mutex.Unlock()
}

func (r *Reader) restart(command string, environ []string) {
	r.event = int32(EvtReady)
	r.startEventPoller()
	success := r.readFromCommand(command, environ)
	r.fin(success)
}

// ReadSource reads data from the default command or from standard input
func (r *Reader) ReadSource(root string, opts walkerOpts, ignores []string) {
	r.startEventPoller()
	var success bool
	if util.IsTty() {
		cmd := os.Getenv("FZF_DEFAULT_COMMAND")
		if len(cmd) == 0 {
			success = r.readFiles(root, opts, ignores)
		} else {
			// We can't export FZF_* environment variables to the default command
			success = r.readFromCommand(cmd, nil)
		}
	} else {
		success = r.readFromStdin()
	}
	r.fin(success)
}

func (r *Reader) feed(src io.Reader) {
	readerSlabSize, ae := strconv.Atoi(os.Getenv("SLAB_KB"))
	if ae != nil {
		readerSlabSize = 128 * 1024
	} else {
		readerSlabSize *= 1024
	}
	readerBufferSize, be := strconv.Atoi(os.Getenv("BUF_KB"))
	if be != nil {
		readerBufferSize = 64 * 1024
	} else {
		readerBufferSize *= 1024
	}

	slab := make([]byte, readerSlabSize)
	pointer := 0
	delim := byte('\n')
	if r.delimNil {
		delim = '\000'
	}
	reader := bufio.NewReaderSize(src, readerBufferSize)

	// We do not put a slice longer than 10% of the slab to reduce fragmentation
	maxBytes := readerBufferSize / 10

	for {
		var frags [][]byte
		fragsLen := 0
		for {
			bytea, err := reader.ReadSlice(delim)
			if err == bufio.ErrBufferFull {
				// Could not find the delimiter in the reader buffer.
				// Need to collect the fragments and merge them later.
				frags = append(frags, bytea)
				fragsLen += len(bytea)
			} else {
				byteaLen := len(bytea)
				if err == nil {
					// No errors. Found the delimiter.
					if util.IsWindows() && byteaLen >= 2 && bytea[byteaLen-2] == byte('\r') {
						bytea = bytea[:byteaLen-2]
						byteaLen -= 2
					} else {
						bytea = bytea[:byteaLen-1]
						byteaLen--
					}
				}

				itemLen := fragsLen + byteaLen
				pointer += itemLen
				var slice []byte
				if itemLen <= maxBytes { // We can use the slab
					// Allocate a new slab if it doesn't fit
					if pointer > readerSlabSize {
						slab = make([]byte, readerSlabSize)
						pointer = itemLen
					}
					slice = slab[pointer-itemLen : pointer]
				} else { // We can't use the slab because the item is too large
					slice = make([]byte, itemLen)
				}

				if len(frags) > 0 {
					// Collect the fragments
					n := 0
					for _, frag := range frags {
						n += copy(slice[n:], frag)
					}
					copy(slice[n:], bytea)
				} else if byteaLen > 0 {
					copy(slice, bytea)
				}
				if (err == nil || itemLen > 0) && r.pusher(slice) {
					atomic.StoreInt32(&r.event, int32(EvtReadNew))
				}
				if err != nil {
					return
				}
				break
			}
		}
	}
}

func (r *Reader) readFromStdin() bool {
	r.feed(os.Stdin)
	return true
}

func (r *Reader) readFiles(root string, opts walkerOpts, ignores []string) bool {
	r.killed = false
	conf := fastwalk.Config{Follow: opts.follow}
	fn := func(path string, de os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		path = filepath.Clean(path)
		if path != "." {
			isDir := de.IsDir()
			if isDir {
				base := filepath.Base(path)
				if !opts.hidden && base[0] == '.' {
					return filepath.SkipDir
				}
				for _, ignore := range ignores {
					if ignore == base {
						return filepath.SkipDir
					}
				}
			}
			if ((opts.file && !isDir) || (opts.dir && isDir)) && r.pusher([]byte(path)) {
				atomic.StoreInt32(&r.event, int32(EvtReadNew))
			}
		}
		r.mutex.Lock()
		defer r.mutex.Unlock()
		if r.killed {
			return context.Canceled
		}
		return nil
	}
	return fastwalk.Walk(&conf, root, fn) == nil
}

func (r *Reader) readFromCommand(command string, environ []string) bool {
	r.mutex.Lock()
	r.killed = false
	r.command = &command
	r.exec = util.ExecCommand(command, true)
	if environ != nil {
		r.exec.Env = environ
	}
	out, err := r.exec.StdoutPipe()
	if err != nil {
		r.mutex.Unlock()
		return false
	}
	err = r.exec.Start()
	r.mutex.Unlock()
	if err != nil {
		return false
	}
	r.feed(out)
	return r.exec.Wait() == nil
}
