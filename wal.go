package wal

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/golog"
	"github.com/golang/snappy"
)

const (
	sentinel          = 0
	defaultFileBuffer = 2 << 16
	compressedSuffix  = ".snappy"
)

var (
	maxSegmentSize = int64(104857600)
	encoding       = binary.BigEndian
	sentinelBytes  = make([]byte, 4) // same as 0
)

type filebased struct {
	dir          string
	file         *os.File
	compressed   bool
	fileSequence int64
	position     int64
	fileFlags    int
	h            hash.Hash32
	log          golog.Logger
}

func (fb *filebased) openFile() error {
	var err error
	if fb.file != nil {
		err = fb.file.Close()
		if err != nil {
			fb.log.Errorf("Unable to close existing file %v: %v", fb.file.Name(), err)
		}
	}

	fb.compressed = false
	fb.file, err = os.OpenFile(fb.filename(), fb.fileFlags, 0600)

	if os.IsNotExist(err) {
		// Try compressed version
		fb.compressed = true
		fb.file, err = os.OpenFile(fb.filename()+compressedSuffix, fb.fileFlags, 0600)
	}

	if err == nil {
		fb.log.Debugf("Opened %v", fb.filename())
	}
	return err
}

func (fb *filebased) filename() string {
	return filepath.Join(fb.dir, sequenceToFilename(fb.fileSequence))
}

// WAL provides a simple write-ahead log backed by a single file on disk. It is
// safe to write to a single WAL from multiple goroutines.
type WAL struct {
	filebased
	syncImmediate bool
	writer        *bufio.Writer
	mx            sync.RWMutex
}

// Open opens a WAL in the given directory. It will be force synced to disk
// every syncInterval. If syncInterval is 0, it will force sync on every write
// to the WAL.
func Open(dir string, syncInterval time.Duration) (*WAL, error) {
	wal := &WAL{
		filebased: filebased{
			dir:       dir,
			fileFlags: os.O_CREATE | os.O_APPEND | os.O_WRONLY,
			h:         newHash(),
			log:       golog.LoggerFor("wal"),
		},
	}
	err := wal.advance()
	if err != nil {
		return nil, err
	}

	if syncInterval <= 0 {
		wal.syncImmediate = true
	} else {
		go wal.sync(syncInterval)
	}

	return wal, nil
}

// Latest() returns the latest entry in the WAL along with its offset
func (wal *WAL) Latest() ([]byte, Offset, error) {
	files, err := ioutil.ReadDir(wal.dir)
	if err != nil {
		return nil, nil, fmt.Errorf("Unable to list log files to get latest value: %v", err)
	}

	lastSeq := int64(0)
	for i := len(files) - 1; i >= 0; i-- {
		file := files[i]
		filename := file.Name()
		fileSequence := filenameToSequence(filename)
		if fileSequence == lastSeq {
			// Duplicate file (compressed vs uncompressed), ignore
			continue
		}

		var r io.Reader
		r, err = os.OpenFile(filepath.Join(wal.dir, filename), os.O_RDONLY, 0600)
		if err != nil {
			return nil, nil, fmt.Errorf("Unable to open WAL file %v: %v", filename, err)
		}
		if strings.HasSuffix(filename, compressedSuffix) {
			r = snappy.NewReader(r)
		} else {
			r = bufio.NewReaderSize(r, defaultFileBuffer)
		}

		h := newHash()
		position := int64(0)
		var data []byte
		for {
			headBuf := make([]byte, 8)
			_, err := io.ReadFull(r, headBuf)
			if err != nil {
				// upon encountering a read error, break, as we've found the end of the latest segment
				break
			}

			length := int64(encoding.Uint32(headBuf))
			checksum := uint32(encoding.Uint32(headBuf[4:]))
			b := make([]byte, length)
			_, err = io.ReadFull(r, b)
			if err != nil {
				// upon encountering a read error, break, as we've found the end of the latest segment
				break
			}
			h.Reset()
			h.Write(b)
			if h.Sum32() != checksum {
				// checksum failure means we've hit a corrupted entry, so we're at the end
				break
			}

			data = b
			position += 8 + length
		}

		if position > 0 {
			// We found a valid entry in the current file, return
			return data, newOffset(fileSequence, position), nil
		}

		lastSeq = fileSequence
	}

	// No files found with a valid entry, return nil data and offset
	return nil, nil, nil
}

// Write atomically writes one or more buffers to the WAL.
func (wal *WAL) Write(bufs ...[]byte) (int, error) {
	wal.mx.Lock()
	defer wal.mx.Unlock()

	length := 0
	for _, b := range bufs {
		length += len(b)
	}
	if length == 0 {
		return 0, nil
	}

	wal.h.Reset()
	for _, buf := range bufs {
		wal.h.Write(buf)
	}

	headerBuf := make([]byte, 4)

	// Write length
	encoding.PutUint32(headerBuf, uint32(length))
	n, err := wal.writer.Write(headerBuf)
	wal.position += int64(n)
	if err != nil {
		return 0, err
	}

	// Write checksum
	encoding.PutUint32(headerBuf, wal.h.Sum32())
	n, err = wal.writer.Write(headerBuf)
	wal.position += int64(n)
	if err != nil {
		return 0, err
	}

	for _, b := range bufs {
		n, err = wal.writer.Write(b)
		if err != nil {
			return 0, err
		}
		wal.position += int64(n)
	}

	if wal.syncImmediate {
		wal.doSync()
	}

	if wal.position >= maxSegmentSize {
		// Write sentinel length to mark end of file
		_, err = wal.writer.Write(sentinelBytes)
		if err != nil {
			return 0, err
		}
		err = wal.writer.Flush()
		if err != nil {
			return 0, err
		}
		err = wal.advance()
		if err != nil {
			return n, fmt.Errorf("Unable to advance to next file: %v", err)
		}
	}

	return n, nil
}

// TruncateBefore removes all data prior to the given offset from disk.
func (wal *WAL) TruncateBefore(o Offset) error {
	files, err := ioutil.ReadDir(wal.dir)
	if err != nil {
		return fmt.Errorf("Unable to list log files to delete: %v", err)
	}

	cutoff := sequenceToFilename(o.FileSequence())
	for i, file := range files {
		if i == len(files)-1 || file.Name() >= cutoff {
			// Files are sorted by name, if we've gotten past the cutoff or
			// encountered the last (active) file, don't bother continuing.
			break
		}
		rmErr := os.Remove(filepath.Join(wal.dir, file.Name()))
		if rmErr != nil {
			return rmErr
		}
		wal.log.Debugf("Removed WAL file %v", filepath.Join(wal.dir, file.Name()))
	}

	return nil
}

// TruncateBeforeTime truncates WAL data prior to the given timestamp.
func (wal *WAL) TruncateBeforeTime(ts time.Time) error {
	return wal.TruncateBefore(newOffset(tsToFileSequence(ts), 0))
}

// CompressBefore compresses all data prior to the given offset on disk.
func (wal *WAL) CompressBefore(o Offset) error {
	files, err := ioutil.ReadDir(wal.dir)
	if err != nil {
		return fmt.Errorf("Unable to list log files to delete: %v", err)
	}

	cutoff := sequenceToFilename(o.FileSequence())
	for i, file := range files {
		if i == len(files)-1 || file.Name() >= cutoff {
			// Files are sorted by name, if we've gotten past the cutoff or
			// encountered the last (active) file, don't bother continuing.
			break
		}
		infile := filepath.Join(wal.dir, file.Name())
		outfile := infile + compressedSuffix
		if strings.HasSuffix(file.Name(), compressedSuffix) {
			// Already compressed
			continue
		}
		in, err := os.OpenFile(infile, os.O_RDONLY, 0600)
		if err != nil {
			return fmt.Errorf("Unable to open input file %v for compression: %v", infile, err)
		}
		defer in.Close()
		out, err := ioutil.TempFile("", "")
		if err != nil {
			return fmt.Errorf("Unable to open temp file to compress %v: %v", infile, err)
		}
		defer out.Close()
		defer os.Remove(out.Name())
		compressedOut := snappy.NewWriter(out)
		_, err = io.Copy(compressedOut, bufio.NewReaderSize(in, defaultFileBuffer))
		if err != nil {
			return fmt.Errorf("Unable to compress %v: %v", infile, err)
		}
		err = compressedOut.Close()
		if err != nil {
			return fmt.Errorf("Unable to finalize compression of %v: %v", infile, err)
		}
		err = out.Close()
		if err != nil {
			return fmt.Errorf("Unable to close compressed output %v: %v", outfile, err)
		}
		err = os.Rename(out.Name(), outfile)
		if err != nil {
			return fmt.Errorf("Unable to move compressed output %v to final destination %v: %v", out.Name(), outfile, err)
		}
		err = os.Remove(infile)
		if err != nil {
			return fmt.Errorf("Unable to remove uncompressed file %v: %v", infile, err)
		}
		wal.log.Debugf("Compressed WAL file %v", infile)
	}

	return nil
}

// CompressBeforeTime compresses all data prior to the given offset on disk.
func (wal *WAL) CompressBeforeTime(ts time.Time) error {
	return wal.CompressBefore(newOffset(tsToFileSequence(ts), 0))
}

// Close closes the wal, including flushing any unsaved writes.
func (wal *WAL) Close() error {
	flushErr := wal.writer.Flush()
	syncErr := wal.file.Sync()
	closeErr := wal.file.Close()
	if flushErr != nil {
		return flushErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (wal *WAL) advance() error {
	wal.fileSequence = newFileSequence()
	wal.position = 0
	err := wal.openFile()
	if err == nil {
		wal.writer = bufio.NewWriterSize(wal.file, defaultFileBuffer)
	}
	return err
}

func (wal *WAL) sync(syncInterval time.Duration) {
	for {
		time.Sleep(syncInterval)
		wal.mx.Lock()
		wal.doSync()
		wal.mx.Unlock()
	}
}

func (wal *WAL) doSync() {
	err := wal.writer.Flush()
	if err != nil {
		wal.log.Errorf("Unable to flush wal: %v", err)
		return
	}
	err = wal.file.Sync()
	if err != nil {
		wal.log.Errorf("Unable to sync wal: %v", err)
	}
}

func (wal *WAL) hasMovedBeyond(fileSequence int64) bool {
	wal.mx.RLock()
	hasMovedBeyond := wal.fileSequence > fileSequence
	wal.mx.RUnlock()
	return hasMovedBeyond
}

// Reader allows reading from a WAL. It is NOT safe to read from a single Reader
// from multiple goroutines.
type Reader struct {
	filebased
	wal    *WAL
	reader io.Reader
	buf    []byte
}

// NewReader constructs a new Reader for reading from this WAL starting at the
// given offset. The returned Reader is NOT safe for use from multiple
// goroutines. Name is just a label for the reader used during logging.
func (wal *WAL) NewReader(name string, offset Offset) (*Reader, error) {
	r := &Reader{
		filebased: filebased{
			dir:       wal.dir,
			fileFlags: os.O_RDONLY,
			h:         newHash(),
			log:       golog.LoggerFor("wal." + name),
		},
		wal: wal,
	}
	if offset != nil {
		offsetString := sequenceToFilename(offset.FileSequence())
		if offsetString[0] != '0' {
			wal.log.Debugf("Converting legacy offset")
			offset = newOffset(offset.FileSequence()/1000, offset.Position())
		}

		files, err := ioutil.ReadDir(wal.dir)
		if err != nil {
			return nil, fmt.Errorf("Unable to list existing log files: %v", err)
		}

		cutoff := sequenceToFilename(offset.FileSequence())
		for _, fileInfo := range files {
			if fileInfo.Name() >= cutoff {
				// Found exist or more recent WAL file
				r.fileSequence = filenameToSequence(fileInfo.Name())
				if r.fileSequence == offset.FileSequence() {
					// Exact match, start at right position
					r.position = offset.Position()
				} else {
					// Newer WAL file, start at beginning
					r.position = 0
				}
				openErr := r.open()
				if openErr != nil {
					return nil, fmt.Errorf("Unable to open existing log file at %v: %v", fileInfo.Name(), openErr)
				}
				break
			}
		}
	}

	if r.file == nil {
		// Didn't find WAL file, advance
		err := r.advance()
		if err != nil {
			return nil, fmt.Errorf("Unable to advance initially: %v", err)
		}
		wal.log.Debugf("Replaying log starting at %v", r.file.Name())
	}
	return r, nil
}

// Read reads the next chunk from the WAL, blocking until one is available.
// Note - the returned byte array is only safe to use until the next call to
// Read(), at which point its contents will get overwritten. So, make a copy of
// the bytes that you need.
func (r *Reader) Read() ([]byte, error) {
	for {
		length, err := r.readHeader()
		if err != nil {
			return nil, err
		}
		checksum, err := r.readHeader()
		if err != nil {
			return nil, err
		}
		data, err := r.readData(length)
		if data != nil || err != nil {
			if data != nil {
				r.h.Reset()
				r.h.Write(data)
				if checksum != int(r.h.Sum32()) {
					r.log.Errorf("Checksum mismatch, skipping entry")
					continue
				}
			}
			return data, err
		}
	}
}

func (r *Reader) readHeader() (int, error) {
	headBuf := make([]byte, 4)
top:
	for {
		length := 0
		read := 0

		for {
			n, err := r.reader.Read(headBuf[read:])
			read += n
			r.position += int64(n)
			if err != nil && err.Error() == "EOF" && read < 4 {
				if r.wal.hasMovedBeyond(r.fileSequence) {
					if read > 0 {
						r.log.Errorf("Out of data to read after reading %d, and WAL has moved beyond %d. Assuming WAL at %v corrupted. Advancing and continuing.", r.position, r.fileSequence, r.filename())
					}
					advanceErr := r.advance()
					if advanceErr != nil {
						return 0, advanceErr
					}
					continue top
				}
				// No newer log files, continue trying to read from this one
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if err != nil {
				r.log.Errorf("Unexpected error reading header from WAL file %v: %v", r.filename(), err)
				break
			}
			if read == 4 {
				length = int(encoding.Uint32(headBuf))
				break
			}
		}

		if length > sentinel {
			return length, nil
		}

		err := r.advance()
		if err != nil {
			return 0, err
		}
	}
}

func (r *Reader) readData(length int) ([]byte, error) {
	// Grow buffer
	if cap(r.buf) < length {
		r.buf = make([]byte, length)
	}

	// Set buffer length
	r.buf = r.buf[:length]

	// Read into buffer
	read := 0
	for {
		n, err := r.reader.Read(r.buf[read:])
		read += n
		r.position += int64(n)
		if err != nil && err.Error() == "EOF" && read < length {
			if r.wal.hasMovedBeyond(r.fileSequence) {
				r.log.Errorf("Out of data to read after reading %d, and WAL has moved beyond %d. Assuming WAL at %v corrupted. Advancing and continuing.", r.position, r.fileSequence, r.filename())
				advanceErr := r.advance()
				if advanceErr != nil {
					return nil, advanceErr
				}
				return nil, nil
			}
			// No newer log files, continue trying to read from this one
			time.Sleep(50 * time.Millisecond)
			continue
		}

		if err != nil {
			r.log.Errorf("Unexpected error reading data from WAL file %v: %v", r.filename(), err)
			return nil, nil
		}

		if read == length {
			return r.buf, nil
		}
	}
}

// Offset returns the furthest Offset read by this Reader. It is NOT safe to
// call this concurrently with Read().
func (r *Reader) Offset() Offset {
	return newOffset(r.fileSequence, r.position)
}

// Close closes the Reader.
func (r *Reader) Close() error {
	return r.file.Close()
}

func (r *Reader) open() error {
	err := r.openFile()
	if err != nil {
		return err
	}
	if r.compressed {
		r.reader = snappy.NewReader(r.file)
	} else {
		r.reader = bufio.NewReaderSize(r.file, defaultFileBuffer)
	}
	if r.position > 0 {
		// Read to the correct offset
		// Note - we cannot just seek on the file because the data is compressed and
		// the recorded position does not correspond to a file offset.
		_, seekErr := io.CopyN(ioutil.Discard, r.reader, r.position)
		if seekErr != nil {
			return seekErr
		}
	}
	return nil
}

func (r *Reader) advance() error {
	r.log.Debugf("Advancing in %v", r.dir)
	for {
		files, err := ioutil.ReadDir(r.dir)
		if err != nil {
			return fmt.Errorf("Unable to list existing log files: %v", err)
		}

		cutoff := sequenceToFilename(r.fileSequence)
		for _, fileInfo := range files {
			seq := filenameToSequence(fileInfo.Name())
			if seq == r.fileSequence {
				// Duplicate WAL segment (i.e. compressed vs uncompressed), ignore
				continue
			}
			if fileInfo.Name() > cutoff {
				// Files are sorted by name, if we've gotten past the cutoff, don't bother
				// continuing
				r.position = 0
				r.fileSequence = seq
				return r.open()
			}
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func newFileSequence() int64 {
	return tsToFileSequence(time.Now())
}

func tsToFileSequence(ts time.Time) int64 {
	return ts.UnixNano() / 1000
}

func sequenceToFilename(seq int64) string {
	return fmt.Sprintf("%019d", seq)
}

func filenameToSequence(filename string) int64 {
	_, filePart := filepath.Split(filename)
	filePart = strings.TrimSuffix(filePart, compressedSuffix)
	seq, err := strconv.ParseInt(filePart, 10, 64)
	if err != nil {
		fmt.Printf("Unparseable filename '%v': %v\n", filename, err)
		return 0
	}
	return seq
}

func newHash() hash.Hash32 {
	return crc32.New(crc32.MakeTable(crc32.Castagnoli))
}
