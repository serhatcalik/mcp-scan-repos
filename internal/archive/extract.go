package archive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrUnsafeArchive = errors.New("unsafe archive")

type Limits struct {
	MaxArchiveBytes   int64
	MaxExtractedBytes int64
	MaxFiles          int
}

func ExtractTarGz(src io.Reader, dest string, limits Limits) error {
	var bounded *io.LimitedReader
	if limits.MaxArchiveBytes > 0 {
		bounded = &io.LimitedReader{R: src, N: limits.MaxArchiveBytes + 1}
		src = bounded
	}
	gz, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	defer gz.Close()
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	var total int64
	var files int
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// Verify gzip's checksum without allowing an unbounded trailing payload.
			tail, err := io.Copy(io.Discard, io.LimitReader(gz, (1<<20)+1))
			if err != nil {
				return err
			}
			if tail > 1<<20 {
				return fmt.Errorf("%w: excessive trailing data", ErrUnsafeArchive)
			}
			if bounded != nil {
				if _, err := io.Copy(io.Discard, bounded); err != nil {
					return err
				}
				if bounded.N == 0 {
					return fmt.Errorf("%w: archive bytes exceeded", ErrUnsafeArchive)
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		files++
		if limits.MaxFiles > 0 && files > limits.MaxFiles {
			return fmt.Errorf("%w: too many files", ErrUnsafeArchive)
		}
		// Reject Windows paths too, even when the worker runs on Linux.
		if strings.HasPrefix(h.Name, "/") || strings.Contains(h.Name, "\\") || strings.Contains(h.Name, ":") {
			return fmt.Errorf("%w: unsafe path", ErrUnsafeArchive)
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(filepath.Separator)) || name == ".." {
			return fmt.Errorf("%w: path traversal %q", ErrUnsafeArchive, h.Name)
		}
		target := filepath.Join(root, name)
		if !strings.HasPrefix(target, root+string(filepath.Separator)) && target != root {
			return fmt.Errorf("%w: path outside root %q", ErrUnsafeArchive, h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if h.Size < 0 || limits.MaxExtractedBytes > 0 && h.Size > limits.MaxExtractedBytes-total {
				return fmt.Errorf("%w: extracted bytes exceeded", ErrUnsafeArchive)
			}
			total += h.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(f, tr, h.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("%w: unsupported entry type %q", ErrUnsafeArchive, h.Name)
		}
	}
}
