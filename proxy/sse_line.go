package proxy

import (
	"bufio"
	"errors"
)

// SSE wire limits are resource bounds, not a declaration that larger events
// are invalid upstream. Base64 media and large tool results can exceed them.
const maxSSELineBytes = 8 << 20

const maxSSEEventBytes = 8 << 20

var errSSELineTooLong = errors.New("upstream SSE line exceeds limit")

var errSSEEventTooLarge = errors.New("upstream SSE event exceeds limit")

// readBoundedSSELine 读取一行，内存占用不超过 limit（不含 bufio 自身缓冲）。
func readBoundedSSELine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return nil, errSSELineTooLong
		}
		line = append(line, fragment...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}
