package proxy

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

func TestReadSSEDataLineBoundsWholeEvent(t *testing.T) {
	line := "data: " + strings.Repeat("x", 1<<20) + "\n"
	data, err := readSSEDataLine(bufio.NewReader(strings.NewReader(strings.Repeat(line, 10) + "\n")))
	if !errors.Is(err, errSSEEventTooLarge) || len(data) != 0 {
		t.Fatalf("len=%d err=%v, want rejected aggregate event", len(data), err)
	}
}

func TestReadSSEDataLineDoesNotHideOversizeLine(t *testing.T) {
	input := "data: first\n" + "data: " + strings.Repeat("x", maxSSELineBytes) + "\n\n"
	data, err := readSSEDataLine(bufio.NewReader(strings.NewReader(input)))
	if !errors.Is(err, errSSELineTooLong) || len(data) != 0 {
		t.Fatalf("len=%d err=%v, want propagated line error", len(data), err)
	}
}

func TestReadSSEDataLinePreservesMultilineEvent(t *testing.T) {
	data, err := readSSEDataLine(bufio.NewReader(strings.NewReader("data: first\ndata: second\n\n")))
	if err != nil || string(data) != "first\nsecond" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

func TestReadBoundedSSELine(t *testing.T) {
	long := strings.Repeat("a", 10000) + "\n"
	reader := bufio.NewReaderSize(strings.NewReader(long), 16)
	line, err := readBoundedSSELine(reader, maxSSELineBytes)
	if err != nil || len(line) != len(long) {
		t.Fatalf("len=%d err=%v, want %d bytes", len(line), err, len(long))
	}

	reader = bufio.NewReaderSize(strings.NewReader("123456789\n"), 4)
	if _, err := readBoundedSSELine(reader, 8); err != errSSELineTooLong {
		t.Fatalf("err = %v, want errSSELineTooLong", err)
	}

	reader = bufio.NewReaderSize(strings.NewReader("12345678\n"), 4)
	if line, err := readBoundedSSELine(reader, 9); err != nil || string(line) != "12345678\n" {
		t.Fatalf("line=%q err=%v", line, err)
	}
}
