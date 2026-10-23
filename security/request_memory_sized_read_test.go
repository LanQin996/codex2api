package security

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestSizedRequestReadMatchesReadAll(t *testing.T) {
	const limit = 64
	cases := []struct {
		name     string
		body     string
		declared int64
	}{
		{name: "exact", body: strings.Repeat("a", 40), declared: 40},
		{name: "declared too small", body: strings.Repeat("b", 40), declared: 8},
		{name: "declared too large", body: strings.Repeat("c", 10), declared: 40},
		{name: "at limit", body: strings.Repeat("d", limit), declared: limit},
		{name: "over limit", body: strings.Repeat("e", limit+20), declared: limit},
		{name: "empty", body: "", declared: 1},
	}
	readers := map[string]func(string) io.Reader{
		"plain":    func(s string) io.Reader { return strings.NewReader(s) },
		"one byte": func(s string) io.Reader { return iotest.OneByteReader(strings.NewReader(s)) },
		"data+EOF": func(s string) io.Reader { return iotest.DataErrReader(strings.NewReader(s)) },
	}
	for _, tc := range cases {
		for readerName, newReader := range readers {
			want, wantErr := io.ReadAll(io.LimitReader(newReader(tc.body), limit+1))
			budget := &requestMemoryBudget{RequestMemorySnapshot: RequestMemorySnapshot{LimitBytes: 1 << 20}}
			reservation, _ := budget.acquire(tc.declared)
			got, err := readRequestMemoryBounded(newReader(tc.body), limit, reservation, tc.declared)
			if !bytes.Equal(got, want) || !errors.Is(err, wantErr) {
				t.Fatalf("%s/%s: got %q, %v; want %q, %v", tc.name, readerName, got, err, want, wantErr)
			}
			if wantCharge := max(int64(len(want)), tc.declared); budget.UsedBytes != wantCharge {
				t.Fatalf("%s/%s: charged %d bytes, want %d", tc.name, readerName, budget.UsedBytes, wantCharge)
			}
			reservation.Release()
		}
	}
}

func TestSizedRequestReadPropagatesReaderErrors(t *testing.T) {
	failure := errors.New("connection reset")
	reader := io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(failure))
	got, err := readRequestMemoryBounded(reader, 64, nil, 32)
	if string(got) != "partial" || !errors.Is(err, failure) {
		t.Fatalf("got %q, %v; want partial data and the reader error", got, err)
	}
}

func BenchmarkSizedRequestRead(b *testing.B) {
	body := bytes.Repeat([]byte("x"), 10<<20)
	limit := int64(48 << 20)
	b.Run("ReadAll", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = readRequestMemoryBounded(bytes.NewReader(body), limit, nil, 0)
		}
	})
	b.Run("ContentLength", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = readRequestMemoryBounded(bytes.NewReader(body), limit, nil, int64(len(body)))
		}
	})
}
