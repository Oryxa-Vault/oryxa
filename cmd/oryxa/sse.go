package main

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// sseReader pulls one data payload at a time out of an SSE stream.
//
// Small on purpose: the wire format is a handful of line prefixes, and the one
// property that matters here is that a multi-line data field is rejoined rather
// than delivered as fragments.
type sseReader struct {
	sc *bufio.Scanner
}

func newSSEReader(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	return &sseReader{sc: sc}
}

func (s *sseReader) next() (json.RawMessage, error) {
	var data []string
	for s.sc.Scan() {
		line := strings.TrimRight(s.sc.Text(), "\r")
		switch {
		case line == "":
			if len(data) == 0 {
				continue // keep-alive or a comment-only frame
			}
			return json.RawMessage(strings.Join(data, "\n")), nil
		case strings.HasPrefix(line, ":"):
			// comment
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := s.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}
