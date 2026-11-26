package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	SampleRate              = 16000
	Channels                = 1
	BytesPerSample          = 2
	ChunkDurationMs         = 100
	ChunkBytes              = SampleRate * Channels * BytesPerSample * ChunkDurationMs / 1000 // 3200 bytes
	DefaultPreRollMaxChunks = 100
	maxStderrBytes          = 4096
)

type AudioSource interface {
	ReadChunk() ([]byte, error)
	Close() error
}

// BufferedAudioSource starts capturing audio immediately into a bounded ring
// buffer so pre-roll speech during connection setup is preserved until drained.
type BufferedAudioSource struct {
	src           AudioSource
	maxChunks     int
	mu            sync.Mutex
	queue         [][]byte
	captureErr    error
	captureDone   bool
	stopRequested bool
	closed        bool
	readyCh       chan struct{}
	stopOnce      sync.Once
	closeOnce     sync.Once
	srcCloseOnce  sync.Once
	doneCh        chan struct{}
}

// NewBufferedAudioSource wraps src and immediately starts background capture
// with a bounded FIFO queue of at most maxChunks chunks (dropping oldest on overflow).
func NewBufferedAudioSource(src AudioSource, maxChunks int) *BufferedAudioSource {
	if maxChunks <= 0 {
		maxChunks = DefaultPreRollMaxChunks
	}
	b := &BufferedAudioSource{
		src:       src,
		maxChunks: maxChunks,
		readyCh:   make(chan struct{}, 1),
		doneCh:    make(chan struct{}),
	}
	go b.captureLoop()
	return b
}

func (b *BufferedAudioSource) signalReady() {
	select {
	case b.readyCh <- struct{}{}:
	default:
	}
}

func (b *BufferedAudioSource) closeSource() {
	b.srcCloseOnce.Do(func() {
		if b.src != nil {
			_ = b.src.Close()
		}
	})
}

func (b *BufferedAudioSource) captureLoop() {
	defer close(b.doneCh)
	if b.src == nil {
		b.mu.Lock()
		b.captureDone = true
		b.signalReady()
		b.mu.Unlock()
		return
	}

	for {
		b.mu.Lock()
		stopped := b.closed || b.stopRequested
		b.mu.Unlock()
		if stopped {
			return
		}

		chunk, err := b.src.ReadChunk()
		if len(chunk) > 0 {
			b.mu.Lock()
			if !b.closed {
				if len(b.queue) >= b.maxChunks {
					b.queue[0] = nil
					b.queue = b.queue[1:]
				}
				b.queue = append(b.queue, chunk)
				b.signalReady()
			}
			b.mu.Unlock()
		}

		if err != nil {
			b.mu.Lock()
			if !b.stopRequested && !b.closed && !errors.Is(err, io.EOF) {
				b.captureErr = err
			}
			b.captureDone = true
			b.signalReady()
			b.mu.Unlock()
			return
		}
	}
}

// ReadChunkContext pops the next buffered chunk or waits until one arrives,
// ctx is cancelled, abortCh closes, or capture finishes.
func (b *BufferedAudioSource) ReadChunkContext(ctx context.Context, abortCh <-chan struct{}) ([]byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return nil, io.EOF
		}
		if len(b.queue) > 0 {
			c := b.queue[0]
			b.queue[0] = nil
			b.queue = b.queue[1:]
			if len(b.queue) == 0 {
				b.queue = b.queue[:0]
			}
			b.mu.Unlock()
			return c, nil
		}
		if b.captureDone || b.stopRequested {
			err := b.captureErr
			b.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-abortCh:
			return nil, io.EOF
		case <-b.readyCh:
		}
	}
}

// ReadChunk implements AudioSource by delegating to ReadChunkContext with a background context.
func (b *BufferedAudioSource) ReadChunk() ([]byte, error) {
	return b.ReadChunkContext(context.Background(), nil)
}

// StopCapture stops capturing new chunks from the underlying source while
// preserving already-buffered chunks so the consumer can drain them before io.EOF.
func (b *BufferedAudioSource) StopCapture() {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		b.stopRequested = true
		b.signalReady()
		b.mu.Unlock()
		b.closeSource()
	})
}

// Close immediately discards any buffered chunks, closes the underlying source,
// and waits for the background capture goroutine to exit.
func (b *BufferedAudioSource) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.queue = nil
		b.signalReady()
		b.mu.Unlock()
		b.closeSource()
		<-b.doneCh
	})
	return nil
}

type boundedBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(bytes.TrimSpace(b.buf))
}

type PipeWireCapture struct {
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	stdout    io.ReadCloser
	stderr    *boundedBuffer
	closed    atomic.Bool
	eof       bool
	closeOnce sync.Once
	waitOnce  sync.Once
	waitErr   error
}

func NewPipeWireCapture(ctx context.Context) (*PipeWireCapture, error) {
	return newCommandCapture(ctx, "pw-record",
		"--raw",
		fmt.Sprintf("--rate=%d", SampleRate),
		fmt.Sprintf("--channels=%d", Channels),
		"--format=s16",
		"-",
	)
}

func newCommandCapture(ctx context.Context, name string, args ...string) (*PipeWireCapture, error) {
	cmdCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cmdCtx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open stdout pipe for %s: %w", name, err)
	}
	stderrBuf := &boundedBuffer{max: maxStderrBytes}
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}

	return &PipeWireCapture{
		cancel: cancel,
		cmd:    cmd,
		stdout: stdout,
		stderr: stderrBuf,
	}, nil
}

func (p *PipeWireCapture) wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = p.cmd.Wait()
	})
	return p.waitErr
}

func (p *PipeWireCapture) ReadChunk() ([]byte, error) {
	if p.eof || p.closed.Load() {
		return nil, io.EOF
	}

	buf := make([]byte, ChunkBytes)
	n, err := io.ReadFull(p.stdout, buf)
	if err == nil {
		return buf, nil
	}
	if errors.Is(err, io.ErrUnexpectedEOF) && n > 0 {
		p.eof = true
		return buf[:n], nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		p.eof = true
		if p.closed.Load() {
			return nil, io.EOF
		}
		if waitErr := p.wait(); waitErr != nil {
			if msg := p.stderr.String(); msg != "" {
				return nil, fmt.Errorf("audio capture exited: %w (%s)", waitErr, msg)
			}
			return nil, fmt.Errorf("audio capture exited: %w", waitErr)
		}
		return nil, io.EOF
	}
	if p.closed.Load() {
		return nil, io.EOF
	}
	if msg := p.stderr.String(); msg != "" {
		return nil, fmt.Errorf("read audio chunk: %w (%s)", err, msg)
	}
	return nil, err
}

func (p *PipeWireCapture) Close() error {
	var retErr error
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		_ = p.stdout.Close()
		if p.cmd.Process != nil {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
		}
		done := make(chan error, 1)
		go func() {
			done <- p.wait()
		}()
		select {
		case err := <-done:
			retErr = err
		case <-time.After(300 * time.Millisecond):
			if p.cmd.Process != nil {
				_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			}
			p.cancel()
			retErr = <-done
		}
		p.cancel()
	})
	if p.closed.Load() {
		return nil
	}
	return retErr
}
