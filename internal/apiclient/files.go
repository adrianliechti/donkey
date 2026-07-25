package apiclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// FileEntry is one item on an app's disk.
type FileEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"modTime"`
	IsDir   bool      `json:"isDir"`
	IsLink  bool      `json:"isLink"`
}

// DiskFolder maps a folder on the disk to the path the app sees it at.
type DiskFolder struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type FileListing struct {
	Path      string       `json:"path"`
	Entries   []FileEntry  `json:"entries"`
	Truncated bool         `json:"truncated"`
	Folders   []DiskFolder `json:"folders"`
}

func (c *Client) ListFiles(ctx context.Context, space, app, path string) (*FileListing, error) {
	q := url.Values{}
	q.Set("path", path)
	var out FileListing
	err := c.doJSON(ctx, http.MethodGet, appPath(space, app, "/files"), q, nil, &out)
	return &out, err
}

// DownloadFile streams one file's contents to w.
func (c *Client) DownloadFile(ctx context.Context, space, app, path string, w io.Writer) error {
	q := url.Values{}
	q.Set("path", path)
	req, err := c.req(ctx, http.MethodGet, appPath(space, app, "/files/download"), q, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach the donkey server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return apiError(resp)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// UploadFile writes r to the given path on the app's disk.
func (c *Client) UploadFile(ctx context.Context, space, app, path string, r io.Reader) error {
	q := url.Values{}
	q.Set("path", path)
	req, err := c.req(ctx, http.MethodPut, appPath(space, app, "/files"), q, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach the donkey server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return apiError(resp)
	}
	return nil
}

func (c *Client) RemoveFile(ctx context.Context, space, app, path string) error {
	q := url.Values{}
	q.Set("path", path)
	return c.doJSON(ctx, http.MethodDelete, appPath(space, app, "/files"), q, nil, nil)
}

func (c *Client) MoveFile(ctx context.Context, space, app, from, to string) error {
	q := url.Values{}
	q.Set("from", from)
	q.Set("to", to)
	return c.doJSON(ctx, http.MethodPost, appPath(space, app, "/files/move"), q, nil, nil)
}

func (c *Client) MakeFolder(ctx context.Context, space, app, path string) error {
	q := url.Values{}
	q.Set("path", path)
	return c.doJSON(ctx, http.MethodPost, appPath(space, app, "/files/folder"), q, nil, nil)
}
