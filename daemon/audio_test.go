package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func makePCMChunk(amplitude int16, numBytes int) []byte {
	buf := make([]byte, numBytes)
	for i := 0; i+1 < numBytes; i += 2 {
		binary.LittleEndian.PutUint16(buf[i:i+2], uint16(amplitude))
	}
	return buf
}

func TestComputeRMS(t *testing.T) {
	silent := makePCMChunk(0, ChunkBytes)
	if rms := ComputeRMS(silent); rms != 0 {
		t.Fatalf("expected 0 RMS for silence, got %v", rms)
	}

	loud := makePCMChunk(1500, ChunkBytes)
	if rms := ComputeRMS(loud); math.Abs(rms-1500) > 1e-6 {
		t.Fatalf("expected 1500 RMS, got %v", rms)
	}
}

func TestVADDetector(t *testing.T) {
	vad := NewVADDetector(300, 300) // 3 chunks of 100ms
	silent := makePCMChunk(10, ChunkBytes)
	speech := makePCMChunk(1000, ChunkBytes)

	// Silence before any speech should never trigger timeout.
	for i := 0; i < 10; i++ {
		if vad.Feed(silent) {
			t.Fatalf("unexpected VAD timeout before speech at iteration %d", i)
		}
	}

	// Speech detected.
	if vad.Feed(speech) {
		t.Fatal("unexpected timeout during speech")
	}

	// 2 silent chunks (200ms < 300ms), then speech resets silence timer.
	for i := 0; i < 2; i++ {
		if vad.Feed(silent) {
			t.Fatalf("unexpected early timeout before 300ms at chunk %d", i+1)
		}
	}
	if vad.Feed(speech) {
		t.Fatal("unexpected timeout when speech resumed")
	}

	// Now 3 consecutive silent chunks trigger timeout on the 3rd chunk.
	if vad.Feed(silent) {
		t.Fatal("unexpected timeout at 100ms")
	}
	if vad.Feed(silent) {
		t.Fatal("unexpected timeout at 200ms")
	}
	if !vad.Feed(silent) {
		t.Fatal("expected VAD timeout at 300ms")
	}

	// Disabled VAD never triggers.
	disabled := NewVADDetector(300, 0)
	if disabled.Feed(speech) {
		t.Fatal("unexpected timeout")
	}
	for i := 0; i < 20; i++ {
		if disabled.Feed(silent) {
			t.Fatal("disabled VAD must never trigger")
		}
	}
}

func TestCommandCapture_FramingAndEOF(t *testing.T) {
	// Emit 3200 + 500 bytes to verify exact chunking and trailing partial chunk.
	ctx := context.Background()
	cap, err := newCommandCapture(ctx, "sh", "-c", "head -c 3700 /dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cap.Close() }()

	c1, err := cap.ReadChunk()
	if err != nil {
		t.Fatalf("read chunk 1: %v", err)
	}
	if len(c1) != ChunkBytes {
		t.Fatalf("chunk 1 len = %d, want %d", len(c1), ChunkBytes)
	}

	c2, err := cap.ReadChunk()
	if err != nil {
		t.Fatalf("read chunk 2: %v", err)
	}
	if len(c2) != 500 || !bytes.Equal(c2, make([]byte, 500)) {
		t.Fatalf("unexpected chunk 2 len=%d", len(c2))
	}

	c3, err := cap.ReadChunk()
	if err != io.EOF || len(c3) != 0 {
		t.Fatalf("expected io.EOF on chunk 3, got len=%d err=%v", len(c3), err)
	}
}

type fakePreRollAudioSource struct {
	mu          sync.Mutex
	chunks      [][]byte
	idx         int
	afterErr    error
	blockAfter  bool
	drainedCh   chan struct{}
	drainedOnce sync.Once
	closeCh     chan struct{}
	closeOnce   sync.Once
	closed      atomic.Bool
}

func newFakePreRollAudioSource(chunks [][]byte, blockAfter bool, afterErr error) *fakePreRollAudioSource {
	return &fakePreRollAudioSource{
		chunks:     chunks,
		afterErr:   afterErr,
		blockAfter: blockAfter,
		drainedCh:  make(chan struct{}),
		closeCh:    make(chan struct{}),
	}
}

func (f *fakePreRollAudioSource) ReadChunk() ([]byte, error) {
	f.mu.Lock()
	if f.closed.Load() {
		f.mu.Unlock()
		return nil, io.EOF
	}
	if f.idx < len(f.chunks) {
		c := f.chunks[f.idx]
		f.idx++
		f.mu.Unlock()
		return c, nil
	}
	f.mu.Unlock()

	f.drainedOnce.Do(func() {
		close(f.drainedCh)
	})
	if f.blockAfter {
		<-f.closeCh
	}
	if f.afterErr != nil {
		return nil, f.afterErr
	}
	return nil, io.EOF
}

func (f *fakePreRollAudioSource) Close() error {
	f.closeOnce.Do(func() {
		f.closed.Store(true)
		close(f.closeCh)
	})
	return nil
}

func TestBufferedAudioSource_PreRollAndStopDrain(t *testing.T) {
	chunks := [][]byte{
		{1, 1},
		{2, 2},
		{3, 3},
	}
	src := newFakePreRollAudioSource(chunks, true, errors.New("closed by stop"))
	b := NewBufferedAudioSource(src, 10)
	defer func() { _ = b.Close() }()

	// Wait until capture goroutine has buffered all 3 pre-roll chunks before any ReadChunk call.
	select {
	case <-src.drainedCh:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for pre-roll chunks to be captured")
	}

	b.StopCapture()
	if !src.closed.Load() {
		t.Fatal("expected StopCapture to close underlying audio source")
	}

	for i, want := range chunks {
		got, err := b.ReadChunk()
		if err != nil {
			t.Fatalf("ReadChunk %d error: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("chunk %d = %v, want %v", i, got, want)
		}
	}

	got, err := b.ReadChunk()
	if !errors.Is(err, io.EOF) || len(got) != 0 {
		t.Fatalf("expected io.EOF after draining buffered chunks, got len=%d err=%v", len(got), err)
	}
}

func TestBufferedAudioSource_DropOldestOverflow(t *testing.T) {
	chunks := [][]byte{
		{1},
		{2},
		{3},
		{4},
		{5},
	}
	src := newFakePreRollAudioSource(chunks, true, nil)
	b := NewBufferedAudioSource(src, 3)
	defer func() { _ = b.Close() }()

	select {
	case <-src.drainedCh:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for chunks to be captured")
	}

	b.StopCapture()

	wantChunks := [][]byte{{3}, {4}, {5}}
	for i, want := range wantChunks {
		got, err := b.ReadChunk()
		if err != nil {
			t.Fatalf("ReadChunk %d error: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("chunk %d = %v, want %v", i, got, want)
		}
	}

	if got, err := b.ReadChunk(); !errors.Is(err, io.EOF) || len(got) != 0 {
		t.Fatalf("expected io.EOF after overflow drain, got len=%d err=%v", len(got), err)
	}
}

func TestBufferedAudioSource_CloseUnblocksWithoutLeak(t *testing.T) {
	src := newFakePreRollAudioSource(nil, true, errors.New("underlying closed"))
	b := NewBufferedAudioSource(src, 0)

	select {
	case <-src.drainedCh:
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for capture goroutine to enter ReadChunk")
	}

	type readResult struct {
		chunk []byte
		err   error
	}
	readResCh := make(chan readResult, 1)
	go func() {
		c, err := b.ReadChunkContext(context.Background(), nil)
		readResCh <- readResult{chunk: c, err: err}
	}()

	time.Sleep(20 * time.Millisecond)

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- b.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Close blocked")
	}

	select {
	case res := <-readResCh:
		if !errors.Is(res.err, io.EOF) || len(res.chunk) != 0 {
			t.Fatalf("expected ReadChunkContext to unblock with io.EOF, got len=%d err=%v", len(res.chunk), res.err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ReadChunkContext did not unblock on Close")
	}

	select {
	case <-b.doneCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("capture goroutine leaked after Close")
	}
	if !src.closed.Load() {
		t.Fatal("expected underlying source to be closed")
	}
}

func TestBufferedAudioSource_AbortAndCaptureError(t *testing.T) {
	t.Run("abortCh_unblocks_reader", func(t *testing.T) {
		src := newFakePreRollAudioSource(nil, true, nil)
		b := NewBufferedAudioSource(src, 4)
		defer func() { _ = b.Close() }()

		abortCh := make(chan struct{})
		resCh := make(chan error, 1)
		go func() {
			_, err := b.ReadChunkContext(context.Background(), abortCh)
			resCh <- err
		}()
		time.Sleep(20 * time.Millisecond)
		close(abortCh)

		select {
		case err := <-resCh:
			if !errors.Is(err, io.EOF) {
				t.Fatalf("expected io.EOF on abortCh close, got %v", err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatal("ReadChunkContext did not unblock on abortCh")
		}
	})

	t.Run("capture_error_surfaced_after_draining", func(t *testing.T) {
		wantErr := errors.New("pw-record exited 1")
		src := newFakePreRollAudioSource([][]byte{{9, 9}}, false, wantErr)
		b := NewBufferedAudioSource(src, 4)
		defer func() { _ = b.Close() }()

		c, err := b.ReadChunk()
		if err != nil || !bytes.Equal(c, []byte{9, 9}) {
			t.Fatalf("expected queued chunk first, got c=%v err=%v", c, err)
		}
		_, err = b.ReadChunk()
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected capture error %v, got %v", wantErr, err)
		}
	})
}
