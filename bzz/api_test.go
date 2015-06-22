package bzz

import (
	"bytes"
	"io/ioutil"
	"os"
	"path"
	"runtime"
	"testing"
)

var (
	testDir string
	datadir = "/tmp/bzz"
)

func init() {
	_, filename, _, _ := runtime.Caller(1)
	testDir = path.Join(path.Dir(filename), "bzztest")
}

func testApi() (api *Api, err error) {
	os.RemoveAll(datadir)
	api, err = NewLocalApi(datadir)
	if err != nil {
		return
	}
	api.Start(nil, ":0", nil)
	return
}

func TestApiPut(t *testing.T) {
	api, err := testApi()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	expContent := "hello"
	expMimeType := "text/plain"
	expStatus := 0
	expSize := len(expContent)
	bzzhash, err := api.Put(expContent, expMimeType)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	testGet(t, api, bzzhash, []byte(expContent), expMimeType, expStatus, expSize)
}

func testGet(t *testing.T, api *Api, bzzhash string, expContent []byte, expMimeType string, expStatus int, expSize int) {
	content, mimeType, status, size, err := api.Get(bzzhash)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	if !bytes.Equal(content, expContent) {
		t.Errorf("incorrect content. expected '%s...', got '%s...'", string(expContent), string(content))
	}
	if mimeType != expMimeType {
		t.Errorf("incorrect mimeType. expected '%s', got '%s'", expMimeType, mimeType)
	}
	if status != expStatus {
		t.Errorf("incorrect status. expected '%d', got '%d'", expStatus, status)
	}
	if size != expSize {
		t.Errorf("incorrect size. expected '%d', got '%d'", expSize, size)
	}
}

func TestApiDirUpload(t *testing.T) {
	api, err := testApi()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	bzzhash, err := api.Upload(path.Join(testDir, "test0"), "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	content, err := ioutil.ReadFile(path.Join(testDir, "test0", "index.html"))
	testGet(t, api, path.Join(bzzhash, "index.html"), content, "text/html; charset=utf-8", 0, 202)

	content, err = ioutil.ReadFile(path.Join(testDir, "test0", "index.css"))
	testGet(t, api, path.Join(bzzhash, "index.css"), content, "text/css", 0, 132)

	content, err = ioutil.ReadFile(path.Join(testDir, "test0", "img", "logo.png"))
	testGet(t, api, path.Join(bzzhash, "img", "logo.png"), content, "image/png", 0, 18136)

	_, _, _, _, err = api.Get(bzzhash)
	if err == nil {
		t.Errorf("expected error: %v", err)
	}
}

func TestApiDirUploadWithRootFile(t *testing.T) {
	api, err := testApi()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	bzzhash, err := api.Upload(path.Join(testDir, "test0"), "index.html")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	content, err := ioutil.ReadFile(path.Join(testDir, "test0", "index.html"))
	testGet(t, api, bzzhash, content, "text/html; charset=utf-8", 0, 202)
}

func TestApiFileUpload(t *testing.T) {
	api, err := testApi()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	bzzhash, err := api.Upload(path.Join(testDir, "test0", "index.html"), "")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	content, err := ioutil.ReadFile(path.Join(testDir, "test0", "index.html"))
	testGet(t, api, path.Join(bzzhash, "index.html"), content, "text/html; charset=utf-8", 0, 202)
}

func TestApiFileUploadWithRootFile(t *testing.T) {
	api, err := testApi()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}
	bzzhash, err := api.Upload(path.Join(testDir, "test0", "index.html"), "index.html")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
		return
	}

	content, err := ioutil.ReadFile(path.Join(testDir, "test0", "index.html"))
	testGet(t, api, bzzhash, content, "text/html; charset=utf-8", 0, 202)
}