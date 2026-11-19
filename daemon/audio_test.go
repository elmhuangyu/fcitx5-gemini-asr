package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"testing"
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
