package mcp

import (
	"context"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxFile = 16 << 20 // 16 MiB per put_file or get_file

var errNoFiles = errors.New("files are not available yet")

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

func (*server) putFile(context.Context, *sdk.CallToolRequest, putFileIn) (*sdk.CallToolResult, putFileOut, error) {
	return nil, putFileOut{}, errNoFiles
}

func (*server) getFile(context.Context, *sdk.CallToolRequest, getFileIn) (*sdk.CallToolResult, getFileOut, error) {
	return nil, getFileOut{}, errNoFiles
}
