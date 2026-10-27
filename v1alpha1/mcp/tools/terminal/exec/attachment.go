package exec

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Attachment is a file terminal_exec writes on this machine before it sends
// anything, for the command it types to run.
type Attachment struct {
	Path string `json:"path" jsonschema:"where the file goes on this machine: an absolute path; missing directories are made (0700) and a file already there is replaced"`
	Data string `json:"data" jsonschema:"the file's content as a data: URI: data:text/x-shellscript;base64,... or data:,percent-encoded text"`
	Mode string `json:"mode,omitempty" jsonschema:"its permissions in octal, 0600 when left out"`
}

// maxAttachments is the most a call's attachments may hold in all, decoded.
const maxAttachments = 1 << 20

// ErrBadAttachment is an attachment refused before anything is written.
var ErrBadAttachment = errors.New("attachment refused")

// file is an Attachment checked and decoded, ready to write.
type file struct {
	path string
	data []byte
	mode fs.FileMode
}

// prepare checks and decodes every attachment, so a bad one is refused
// before any is written.
func prepare(as []Attachment) ([]file, error) {
	files := make([]file, 0, len(as))
	total := 0
	for i, a := range as {
		bad := func(format string, args ...any) error {
			return fmt.Errorf("%w: attachment %d: %s", ErrBadAttachment, i+1, fmt.Sprintf(format, args...))
		}
		if !filepath.IsAbs(a.Path) {
			return nil, bad("%q is not an absolute path", a.Path)
		}
		if info, err := os.Stat(a.Path); err == nil && info.IsDir() {
			return nil, bad("%s is a directory", a.Path)
		}
		mode := fs.FileMode(0o600)
		if a.Mode != "" {
			m, err := strconv.ParseUint(a.Mode, 8, 32)
			if err != nil || m > 0o777 {
				return nil, bad("mode %q is not octal permissions, 0000 to 0777", a.Mode)
			}
			mode = fs.FileMode(m)
		}
		if total+len(a.Data) > 2*maxAttachments {
			return nil, fmt.Errorf("%w: more than 1 MiB in all", ErrBadAttachment)
		}
		data, err := decode(a.Data)
		if err != nil {
			return nil, bad("%s", strings.TrimPrefix(err.Error(), ErrBadAttachment.Error()+": "))
		}
		if total += len(data); total > maxAttachments {
			return nil, fmt.Errorf("%w: more than 1 MiB in all", ErrBadAttachment)
		}
		files = append(files, file{a.Path, data, mode})
	}
	return files, nil
}

// decode is a data: URI's content (RFC 2397): base64, padded or not, or
// percent-encoded text.
func decode(uri string) ([]byte, error) {
	rest, ok := strings.CutPrefix(uri, "data:")
	meta, payload, found := strings.Cut(rest, ",")
	if !ok || !found {
		return nil, fmt.Errorf("%w: not a data: URI", ErrBadAttachment)
	}
	if strings.HasSuffix(meta, ";base64") {
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(payload)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: not base64: %v", ErrBadAttachment, err)
		}
		return data, nil
	}
	text, err := url.PathUnescape(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: not percent-encoded: %v", ErrBadAttachment, err)
	}
	return []byte(text), nil
}

// write puts each file in place through a temporary file beside it and a
// rename, so a command never runs a file half written.
func write(files []file) error {
	for _, f := range files {
		dir := filepath.Dir(f.path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(dir, ".tunneld-*")
		if err != nil {
			return err
		}
		_, err = tmp.Write(f.data)
		err = errors.Join(err, tmp.Chmod(f.mode), tmp.Close())
		if err == nil {
			err = os.Rename(tmp.Name(), f.path)
		}
		if err != nil {
			_ = os.Remove(tmp.Name())
			return err
		}
	}
	return nil
}
