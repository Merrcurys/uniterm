package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestClipboardTransferChunks(t *testing.T) {
	a := &App{}
	files := []ClipboardFileInfo{{Name: "报告.txt", Size: 6, LastModified: 1700000000000}, {Name: "empty", Size: 0}}
	id, err := a.ClipboardBeginFileTransfer(files)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.ClipboardAbortFileTransfer(id) })
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	if err := a.ClipboardWriteFileChunk(id, 0, 1, encode("abc")); err == nil {
		t.Fatal("accepted a non-contiguous chunk")
	}
	if err := a.ClipboardWriteFileChunk(id, 0, 0, encode("abc")); err != nil {
		t.Fatal(err)
	}
	publish := func(paths []string) error {
		t.Cleanup(func() { os.RemoveAll(filepath.Dir(paths[0])) })
		data, err := os.ReadFile(paths[0])
		if err != nil || string(data) != "abcdef" {
			t.Fatalf("downloaded file = %q, %v", data, err)
		}
		st, err := os.Stat(paths[1])
		if err != nil || st.Size() != 0 {
			t.Fatalf("empty file: %v", err)
		}
		st, err = os.Stat(paths[0])
		if err != nil || st.ModTime().UnixMilli() != files[0].LastModified {
			t.Fatalf("modified time: %v", err)
		}
		return nil
	}
	if err := finishClipboardTransfer(id, publish); err == nil {
		t.Fatal("published an incomplete file")
	}
	if err := a.ClipboardWriteFileChunk(id, 0, 3, encode("defg")); err == nil {
		t.Fatal("accepted data beyond declared size")
	}
	if err := a.ClipboardWriteFileChunk(id, 0, 3, encode("def")); err != nil {
		t.Fatal(err)
	}
	if err := finishClipboardTransfer(id, publish); err != nil {
		t.Fatal(err)
	}
	if err := a.ClipboardWriteFileChunk(id, 0, 0, encode("abc")); err == nil {
		t.Fatal("accepted an expired transfer token")
	}
}

func TestClipboardTransferRejectsPaths(t *testing.T) {
	a := &App{}
	for _, name := range []string{"", ".", "..", "../outside", `..\outside`, "dir/file", `C:\file`, "file:stream", "file\x00.txt"} {
		t.Run(name, func(t *testing.T) {
			if id, err := a.ClipboardBeginFileTransfer([]ClipboardFileInfo{{Name: name}}); err == nil {
				a.ClipboardAbortFileTransfer(id)
				t.Fatalf("accepted unsafe path %q", name)
			}
		})
	}
	if id, err := a.ClipboardBeginFileTransfer([]ClipboardFileInfo{{Name: "File"}, {Name: "file"}}); err == nil {
		a.ClipboardAbortFileTransfer(id)
		t.Fatal("accepted case-insensitive duplicate names")
	}
}

func TestClipboardTransferAbort(t *testing.T) {
	a := &App{}
	id, err := a.ClipboardBeginFileTransfer([]ClipboardFileInfo{{Name: "partial", Size: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	clipboardTransfers.Lock()
	dir := clipboardTransfers.items[id].dir
	clipboardTransfers.Unlock()
	if err := a.ClipboardAbortFileTransfer(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("partial files remain: %v", err)
	}
}
