package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	SampleRate      = 16000
	Channels        = 1
	BytesPerSample  = 2
	ChunkDurationMs = 100
	ChunkBytes      = SampleRate * Channels * BytesPerSample * ChunkDurationMs / 1000 // 3200 bytes
	maxStderrBytes  = 4096
)

type AudioSource interface {
	ReadChunk() ([]byte, error)
	Close() error
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
	return strings.TrimSpace(string(bytes.TrimSpace(b.buf)))
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

// ComputeRMS returns the Root Mean Square amplitude of little-endian 16-bit signed PCM audio.
func ComputeRMS(pcm []byte) float64 {
	numSamples := len(pcm) / BytesPerSample
	if numSamples == 0 {
		return 0
	}
	var sum float64
	for i := 0; i+1 < len(pcm); i += BytesPerSample {
		s := int16(binary.LittleEndian.Uint16(pcm[i : i+BytesPerSample]))
		f := float64(s)
		sum += f * f
	}
	return math.Sqrt(sum / float64(numSamples))
}

type VADDetector struct {
	threshold      float64
	silenceLimitMs int
	hasSpeech      bool
	silenceMs      int
}

func NewVADDetector(threshold float64, silenceLimitMs int) *VADDetector {
	if threshold <= 0 {
		threshold = DefaultVADThreshold
	}
	return &VADDetector{
		threshold:      threshold,
		silenceLimitMs: silenceLimitMs,
	}
}

func (v *VADDetector) Feed(pcm []byte) bool {
	if v.silenceLimitMs <= 0 || len(pcm) < BytesPerSample {
		return false
	}
	chunkMs := (len(pcm) * 1000) / (SampleRate * Channels * BytesPerSample)
	if chunkMs <= 0 {
		chunkMs = 1
	}

	if ComputeRMS(pcm) >= v.threshold {
		v.hasSpeech = true
		v.silenceMs = 0
		return false
	}

	if !v.hasSpeech {
		return false
	}

	v.silenceMs += chunkMs
	return v.silenceMs >= v.silenceLimitMs
}
