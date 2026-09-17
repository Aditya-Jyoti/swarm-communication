package web

import (
	"io/fs"
	"testing"
)

func TestStaticIsRootedAtStaticDir(t *testing.T) {
	// .gitkeep is always present; finding it at the root proves the fs.Sub.
	if _, err := fs.Stat(Static(), ".gitkeep"); err != nil {
		t.Fatalf("Static() is not rooted at static/: %v", err)
	}
}
