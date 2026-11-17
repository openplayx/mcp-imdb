package imdb

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

// tsvReader reads one IMDb dataset file: gzip-compressed, tab-separated,
// one header row, no quoting, "\N" for null.
type tsvReader struct {
	name   string
	f      *os.File
	gz     *gzip.Reader
	r      *bufio.Reader
	line   int
	fields [][]byte
}

func openTSV(path, name string, header []string) (*tsvReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	t := &tsvReader{name: name, f: f, gz: gz, r: bufio.NewReaderSize(gz, 1<<20)}
	got, err := t.next()
	if err != nil {
		t.Close()
		return nil, fmt.Errorf("%s: reading header: %w", name, err)
	}
	cols := make([]string, len(got))
	for i, c := range got {
		cols[i] = string(c)
	}
	if !slices.Equal(cols, header) {
		t.Close()
		return nil, fmt.Errorf("%s: unexpected columns %q (want %q); IMDb changed the dataset format", name, cols, header)
	}
	return t, nil
}

func (t *tsvReader) Close() {
	t.gz.Close()
	t.f.Close()
}

// next returns the fields of the next row; they are only valid until the
// following call. io.EOF at the end.
func (t *tsvReader) next() ([][]byte, error) {
	line, err := t.r.ReadSlice('\n')
	for err == bufio.ErrBufferFull { // very long line: accumulate
		buf := append([]byte(nil), line...)
		var more []byte
		more, err = t.r.ReadSlice('\n')
		line = append(buf, more...)
	}
	if err == io.EOF && len(line) > 0 {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	t.line++
	line = bytes.TrimRight(line, "\r\n")
	t.fields = t.fields[:0]
	for {
		i := bytes.IndexByte(line, '\t')
		if i < 0 {
			t.fields = append(t.fields, line)
			break
		}
		t.fields = append(t.fields, line[:i])
		line = line[i+1:]
	}
	return t.fields, nil
}

// row reads the next row and checks its width.
func (t *tsvReader) row(width int) ([][]byte, error) {
	for {
		f, err := t.next()
		if err != nil {
			return nil, err
		}
		if len(f) == width {
			return f, nil
		}
		if len(f) == 1 && len(f[0]) == 0 {
			continue // blank line
		}
		return nil, fmt.Errorf("%s line %d: %d fields, want %d", t.name, t.line, len(f), width)
	}
}

func isNull(b []byte) bool { return len(b) == 0 || (len(b) == 2 && b[0] == '\\' && b[1] == 'N') }

// str returns nil for null so SQLite stores NULL.
func str(b []byte) any {
	if isNull(b) {
		return nil
	}
	return string(b)
}

func num(b []byte) any {
	if isNull(b) {
		return nil
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return nil
	}
	return n
}

func id(prefix string, b []byte) (int64, bool) {
	if len(b) <= len(prefix) || string(b[:len(prefix)]) != prefix {
		return 0, false
	}
	n, err := strconv.ParseInt(string(b[len(prefix):]), 10, 64)
	return n, err == nil && n > 0
}

// idList turns "nm1,nm2" into "1,2" (stored compactly); nil when empty.
func idList(prefix string, b []byte) any {
	if isNull(b) {
		return nil
	}
	parts := bytes.Split(b, []byte{','})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if n, ok := id(prefix, p); ok {
			out = append(out, strconv.FormatInt(n, 10))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return strings.Join(out, ",")
}

// bitset over positive ids.
type bitset []uint64

func (b *bitset) set(n int64) {
	i := int(n >> 6)
	if i >= len(*b) {
		grown := make([]uint64, i+1+i/4)
		copy(grown, *b)
		*b = grown
	}
	(*b)[i] |= 1 << uint(n&63)
}

func (b bitset) has(n int64) bool {
	i := int(n >> 6)
	return i < len(b) && b[i]&(1<<uint(n&63)) != 0
}
