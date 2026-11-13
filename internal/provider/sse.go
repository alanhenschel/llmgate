package provider

import (
	"bufio"
	"bytes"
	"io"
)

// sseReader parses a text/event-stream body. Lines are unbounded (ReadBytes,
// not Scanner) so large chunks never hit a token limit.
type sseReader struct{ r *bufio.Reader }

func newSSEReader(r io.Reader) *sseReader { return &sseReader{r: bufio.NewReaderSize(r, 16<<10)} }

// next returns the next event with at least one data line.
func (s *sseReader) next() (event string, data []byte, err error) {
	var buf []byte
	have := false
	for {
		line, rerr := s.r.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0 && rerr == nil:
			if have {
				return event, buf, nil
			}
		case len(line) == 0:
			// EOF or error with nothing pending on this line.
			if have && rerr == io.EOF {
				return event, buf, nil
			}
			return "", nil, rerr
		case line[0] == ':':
			// comment / keep-alive
		default:
			field, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			switch string(field) {
			case "event":
				event = string(value)
			case "data":
				if have {
					buf = append(buf, '\n')
				}
				buf = append(buf, value...)
				have = true
			}
			if rerr != nil {
				if have && rerr == io.EOF {
					return event, buf, nil
				}
				return "", nil, rerr
			}
		}
	}
}
