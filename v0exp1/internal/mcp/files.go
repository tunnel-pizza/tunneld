package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxFile = 16 << 20 // 16 MiB per put_file or get_file

type putFileIn struct {
	N             int    `json:"n" jsonschema:"origin index, as origins lists"`
	Path          string `json:"path" jsonschema:"absolute path on the origin"`
	ContentBase64 string `json:"content_base64" jsonschema:"the bytes, base64"`
	Mode          string `json:"mode,omitempty" jsonschema:"octal file mode, default 0644"`
}

type putFileOut struct {
	Bytes int `json:"bytes"`
}

type getFileIn struct {
	N    int    `json:"n" jsonschema:"origin index, as origins lists"`
	Path string `json:"path" jsonschema:"absolute path on the origin"`
}

type getFileOut struct {
	ContentBase64 string `json:"content_base64"`
	Bytes         int    `json:"bytes"`
	Mode          string `json:"mode"`
}

// fileOrigin checks origin n as a place with a filesystem, and path as one
// on it. A program origin's files are tunneld's own, as the user it runs as.
// A container has a filesystem too, but not one this build can reach — that
// comes with the docker provider's Spawner.
func (s *server) fileOrigin(n int, path string) error {
	o, err := s.origin(n)
	if err != nil {
		return err
	}
	if o.Kind != KindProgram {
		return fmt.Errorf("origin %d: files on %s %s origin are not supported yet", n, article(o.Kind), o.Kind)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path %q is not absolute", path)
	}
	return nil
}

// putFile is the put_file tool: bytes to an absolute path, with the mode
// asked for whether the file was there or not.
func (s *server) putFile(_ context.Context, _ *sdk.CallToolRequest, in putFileIn) (*sdk.CallToolResult, putFileOut, error) {
	if err := s.fileOrigin(in.N, in.Path); err != nil {
		return nil, putFileOut{}, err
	}
	data, err := base64.StdEncoding.DecodeString(in.ContentBase64)
	if err != nil {
		return nil, putFileOut{}, fmt.Errorf("content_base64: %w", err)
	}
	if len(data) > maxFile {
		return nil, putFileOut{}, fmt.Errorf("content is %d bytes; at most %d", len(data), maxFile)
	}
	mode := os.FileMode(0o644)
	if in.Mode != "" {
		m, err := strconv.ParseUint(in.Mode, 8, 32)
		if err != nil {
			return nil, putFileOut{}, fmt.Errorf("mode %q is not octal", in.Mode)
		}
		mode = os.FileMode(m)
	}
	if err := os.WriteFile(in.Path, data, mode); err != nil {
		return nil, putFileOut{}, err
	}
	// WriteFile applies the mode only to a file it creates; one that was
	// there keeps its own, so the mode asked for is set either way.
	if err := os.Chmod(in.Path, mode); err != nil {
		return nil, putFileOut{}, err
	}
	s.log.Debug("mcp put_file", "origin", in.N, "path", in.Path, "bytes", len(data))
	return nil, putFileOut{Bytes: len(data)}, nil
}

// getFile is the get_file tool: an absolute path's bytes and mode.
func (s *server) getFile(_ context.Context, _ *sdk.CallToolRequest, in getFileIn) (*sdk.CallToolResult, getFileOut, error) {
	if err := s.fileOrigin(in.N, in.Path); err != nil {
		return nil, getFileOut{}, err
	}
	f, err := os.Open(in.Path)
	if err != nil {
		return nil, getFileOut{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, getFileOut{}, err
	}
	if st.IsDir() {
		return nil, getFileOut{}, fmt.Errorf("%s is a directory", in.Path)
	}
	if st.Size() > maxFile {
		return nil, getFileOut{}, fmt.Errorf("%s is %d bytes; at most %d", in.Path, st.Size(), maxFile)
	}
	// Limited past the stat, for a file that grows while it is read.
	data, err := io.ReadAll(io.LimitReader(f, maxFile))
	if err != nil {
		return nil, getFileOut{}, err
	}
	s.log.Debug("mcp get_file", "origin", in.N, "path", in.Path, "bytes", len(data))
	return nil, getFileOut{
		ContentBase64: base64.StdEncoding.EncodeToString(data),
		Bytes:         len(data),
		Mode:          fmt.Sprintf("%04o", st.Mode().Perm()),
	}, nil
}
