// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mydump

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/br/pkg/storage"
	"github.com/pingcap/tidb/pkg/lightning/worker"
	"github.com/pingcap/tidb/pkg/parser/charset"
	"github.com/pingcap/tidb/pkg/util/logutil"
	"github.com/spkg/bom"
	"go.uber.org/zap"
	"golang.org/x/text/encoding/charmap"
)

var (
	// ErrInsertStatementNotFound is the error that cannot find the insert statement.
	ErrInsertStatementNotFound = errors.New("insert statement not found")
	errInvalidSchemaEncoding   = errors.New("invalid schema encoding")
)

func decodeCharacterSet(data []byte, characterSet string) ([]byte, error) {
	switch characterSet {
	case "binary":
		// do nothing
	case "auto", "utf8mb4":
		if utf8.Valid(data) {
			break
		}
		if characterSet == "utf8mb4" {
			return nil, errInvalidSchemaEncoding
		}
		// try gb18030 next if the encoding is "auto"
		// if we support too many encodings, consider switching strategy to
		// perform `chardet` first.
		fallthrough
	case "gb18030":
		decoded, err := charset.EncodingGB18030Impl.Transform(nil, data, charset.OpDecodeReplace)
		if err != nil {
			return nil, errors.Trace(err)
		}
		// check for U+FFFD to see if decoding contains errors.
		// https://groups.google.com/d/msg/golang-nuts/pENT3i4zJYk/v2X3yyiICwAJ
		if bytes.ContainsRune(decoded, '\ufffd') {
			return nil, errInvalidSchemaEncoding
		}
		data = decoded
	case "latin1":
		// use Windows1252 (not ISO 8859-1) to decode Latin1
		// https://dev.mysql.com/doc/refman/8.0/en/charset-we-sets.html
		decoded, err := charmap.Windows1252.NewDecoder().Bytes(data)
		if err != nil {
			return nil, errors.Trace(err)
		}
		// > Each byte that cannot be transcoded will be represented in the
		// > output by the UTF-8 encoding of '\uFFFD'
		if bytes.ContainsRune(decoded, '\ufffd') {
			return nil, errInvalidSchemaEncoding
		}
		data = decoded
	default:
		return nil, errors.Errorf("Unsupported encoding %s", characterSet)
	}
	return data, nil
}

// ExportStatement exports the SQL statement in the schema file.
func ExportStatement(ctx context.Context, store storage.ExternalStorage,
	sqlFile FileInfo, characterSet string) ([]byte, error) {
	if sqlFile.FileMeta.Compression != CompressionNone {
		compressType, err := ToStorageCompressType(sqlFile.FileMeta.Compression)
		if err != nil {
			return nil, errors.Trace(err)
		}
		store = storage.WithCompression(store, compressType, storage.DecompressConfig{
			ZStdDecodeConcurrency: 1,
		})
	}
	fd, err := store.Open(ctx, sqlFile.FileMeta.Path, nil)
	if err != nil {
		return nil, errors.Trace(err)
	}
	defer fd.Close()

	br := bufio.NewReader(bom.NewReader(fd))

	data := make([]byte, 0, sqlFile.FileMeta.FileSize+1)
	buffer := make([]byte, 0, sqlFile.FileMeta.FileSize+1)
	for {
		line, err := br.ReadBytes('\n')
		if errors.Cause(err) == io.EOF {
			if len(line) == 0 { // it will return EOF if there is no trailing new line.
				break
			}
		} else if err != nil {
			return nil, errors.Trace(err)
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		buffer = append(buffer, line...)
		if buffer[len(buffer)-1] == ';' {
			statement := string(buffer)
			if !(strings.HasPrefix(statement, "/*") && strings.HasSuffix(statement, "*/;")) {
				data = append(data, buffer...)
			}
			buffer = buffer[:0]
		} else {
			buffer = append(buffer, '\n')
		}
	}

	data, err = decodeCharacterSet(data, characterSet)
	if err != nil {
		logutil.Logger(ctx).Error("cannot decode input file, please convert to target encoding manually",
			zap.String("encoding", characterSet),
			zap.String("Path", sqlFile.FileMeta.Path),
		)
		return nil, errors.Annotatef(err, "failed to decode %s as %s", sqlFile.FileMeta.Path, characterSet)
	}
	return data, nil
}

// ReadSeekCloser = Reader + Seeker + Closer
type ReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

// StringReader is a wrapper around *strings.Reader with an additional Close() method
type StringReader struct{ *strings.Reader }

// NewStringReader constructs a new StringReader
func NewStringReader(s string) StringReader {
	return StringReader{Reader: strings.NewReader(s)}
}

// Close implements io.Closer
func (StringReader) Close() error {
	return nil
}

// PooledReader is a throttled reader wrapper, where Read() calls have an upper limit of concurrency
// imposed by the given worker pool.
type PooledReader struct {
	reader    ReadSeekCloser
	ioWorkers *worker.Pool
}

// MakePooledReader constructs a new PooledReader.
func MakePooledReader(reader ReadSeekCloser, ioWorkers *worker.Pool) PooledReader {
	return PooledReader{
		reader:    reader,
		ioWorkers: ioWorkers,
	}
}

// Read implements io.Reader
func (pr PooledReader) Read(p []byte) (n int, err error) {
	if pr.ioWorkers != nil {
		w := pr.ioWorkers.Apply()
		defer pr.ioWorkers.Recycle(w)
	}
	return pr.reader.Read(p)
}

// Seek implements io.Seeker
func (pr PooledReader) Seek(offset int64, whence int) (int64, error) {
	// Seek(0, io.SeekCurrent) is used to get the current offset, which will not cause any Disk I/O.
	if pr.ioWorkers != nil && !(offset == 0 && whence == io.SeekCurrent) {
		w := pr.ioWorkers.Apply()
		defer pr.ioWorkers.Recycle(w)
	}
	return pr.reader.Seek(offset, whence)
}

// Close implements io.Closer
func (pr PooledReader) Close() error {
	return pr.reader.Close()
}

// ReadFull is same as `io.ReadFull(pr)` with less worker recycling
func (pr PooledReader) ReadFull(buf []byte) (n int, err error) {
	if pr.ioWorkers != nil {
		w := pr.ioWorkers.Apply()
		defer pr.ioWorkers.Recycle(w)
	}
	return io.ReadFull(pr.reader, buf)
}
