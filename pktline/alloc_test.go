package pktline

import (
	"bytes"
	"io"
	"testing"
)

// TestWriterAllocs pins the per-packet allocation count: the frame buffer is
// reused, so a warm Writer allocates nothing however many packets it frames.
func TestWriterAllocs(t *testing.T) {
	w := NewWriter(io.Discard)
	payload := bytes.Repeat([]byte{'x'}, 10*MaxSidebandData+17) // 11 packets
	if err := w.Sideband(1, payload); err != nil {
		t.Fatal(err)
	}
	if n := testing.AllocsPerRun(50, func() { _ = w.Sideband(1, payload) }); n != 0 {
		t.Errorf("Sideband: %v allocs per call, want 0", n)
	}
	if n := testing.AllocsPerRun(50, func() { _ = w.Data(payload[:100]) }); n != 0 {
		t.Errorf("Data: %v allocs per call, want 0", n)
	}
}

// TestWriterFrameReuse checks that reusing the frame buffer does not corrupt
// what an underlying writer that copies each packet sees.
func TestWriterFrameReuse(t *testing.T) {
	var got [][]byte
	w := NewWriter(writerFunc(func(p []byte) (int, error) {
		got = append(got, append([]byte(nil), p...))
		return len(p), nil
	}))
	_ = w.Data([]byte("hello\n"))
	_ = w.Sideband(2, []byte("ab"))
	_ = w.Data([]byte("z"))
	want := []string{"000ahello\n", "0007\x02ab", "0005z"}
	if len(got) != len(want) {
		t.Fatalf("got %d packets, want %d", len(got), len(want))
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("packet %d = %q, want %q", i, got[i], want[i])
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
