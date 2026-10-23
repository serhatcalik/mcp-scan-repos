package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	writeArchive(t, &buf, map[string]string{"../evil.txt": "nope"})
	err := ExtractTarGz(bytes.NewReader(buf.Bytes()), t.TempDir(), Limits{MaxExtractedBytes: 1024, MaxFiles: 10})
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive", err)
	}
}

func TestExtractTarGzRejectsSymlink(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	err := ExtractTarGz(bytes.NewReader(buf.Bytes()), t.TempDir(), Limits{MaxExtractedBytes: 1024, MaxFiles: 10})
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive", err)
	}
}

func TestExtractTarGzHonorsSizeLimit(t *testing.T) {
	var buf bytes.Buffer
	writeArchive(t, &buf, map[string]string{"a.txt": "123456"})
	err := ExtractTarGz(bytes.NewReader(buf.Bytes()), t.TempDir(), Limits{MaxExtractedBytes: 3, MaxFiles: 10})
	if !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("error = %v, want ErrUnsafeArchive", err)
	}
}

func TestExtractTarGzWritesRegularFiles(t *testing.T) {
	var buf bytes.Buffer
	writeArchive(t, &buf, map[string]string{"dir/a.txt": "hello"})
	dir := t.TempDir()
	if err := ExtractTarGz(bytes.NewReader(buf.Bytes()), dir, Limits{MaxExtractedBytes: 1024, MaxFiles: 10}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "dir", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("file = %q", got)
	}
}

func writeArchive(t *testing.T, buf *bytes.Buffer, files map[string]string) {
	t.Helper()
	gz := gzip.NewWriter(buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o600}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
}

func TestExtractRejectsUnsafeEntryTypesAndPaths(t *testing.T) {
	for _, header := range []*tar.Header{
		{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "a"},
		{Name: "device", Typeflag: tar.TypeChar},
		{Name: "pipe", Typeflag: tar.TypeFifo},
		{Name: "/absolute", Typeflag: tar.TypeReg},
		{Name: "C:/windows/file", Typeflag: tar.TypeReg},
		{Name: "..\\escape", Typeflag: tar.TypeReg},
	} {
		t.Run(header.Name, func(t *testing.T) {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gz)
			if err := tw.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			_ = tw.Close()
			_ = gz.Close()
			if err := ExtractTarGz(bytes.NewReader(buf.Bytes()), t.TempDir(), Limits{}); !errors.Is(err, ErrUnsafeArchive) {
				t.Fatalf("accepted unsafe entry: %v", err)
			}
		})
	}
}

func TestExtractCompressedLimitIncludesTrailingBytes(t *testing.T) {
	var buf bytes.Buffer
	writeArchive(t, &buf, map[string]string{"a": "hello"})
	limit := buf.Len()
	buf.Write(make([]byte, 100))
	if err := ExtractTarGz(bytes.NewReader(buf.Bytes()), t.TempDir(), Limits{MaxArchiveBytes: int64(limit)}); !errors.Is(err, ErrUnsafeArchive) {
		t.Fatalf("archive limit not enforced: %v", err)
	}
}

func TestExtractRejectsCorruptedGzipChecksum(t *testing.T) {
	var buf bytes.Buffer
	writeArchive(t, &buf, map[string]string{"a": "hello"})
	data := append([]byte(nil), buf.Bytes()...)
	data[len(data)-8] ^= 0xff
	if err := ExtractTarGz(bytes.NewReader(data), t.TempDir(), Limits{}); err == nil {
		t.Fatal("corrupted gzip checksum accepted")
	}
}
