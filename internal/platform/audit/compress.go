package audit

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// compressedExt is the filename suffix a codec appends to a sealed file.
// CompressionNone yields "" (the file is staged verbatim, no re-encode).
func compressedExt(codec string) string {
	switch codec {
	case CompressionZstd:
		return ".zst"
	case CompressionGzip:
		return ".gz"
	default:
		return ""
	}
}

// newCompressWriter wraps out with the codec's streaming encoder. zstd uses a
// multi-MB window versus gzip/DEFLATE's fixed 32 KiB, which is exactly what
// captures the cross-event repetition in agent audit JSONL: every call echoes
// the whole prior conversation, so the same text recurs dozens of times per
// file. gzip's tiny window can't reference a repeat that started >32 KiB back;
// zstd collapses each one to a back-ref. SpeedBetterCompression keeps the
// single-threaded shipper affordable while still landing the window win.
func newCompressWriter(codec string, out io.Writer) (io.WriteCloser, error) {
	switch codec {
	case CompressionGzip:
		return gzip.NewWriter(out), nil
	case CompressionZstd:
		// WithEncoderConcurrency(1) keeps this on one core: compressPass is
		// deliberately single-threaded so audit compression never steals
		// request-serving capacity, but zstd otherwise fans out to GOMAXPROCS
		// workers — and a file can reach RotateMaxBytes (128 MiB default).
		return zstd.NewWriter(out,
			zstd.WithEncoderLevel(zstd.SpeedBetterCompression),
			zstd.WithEncoderConcurrency(1))
	default:
		return nil, fmt.Errorf("audit: unknown compression codec %q", codec)
	}
}

// compressFile streams src → dst with the given codec, so a large file never
// loads into memory. On any error dst is removed, so a partial output is
// never left behind for the uploader to pick up.
func compressFile(codec, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	w, err := newCompressWriter(codec, out)
	if err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if _, err := io.Copy(w, in); err != nil {
		_ = w.Close()
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := w.Close(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}
