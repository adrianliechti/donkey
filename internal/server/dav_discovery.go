package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type davDiscovery struct {
	protocol string
	query    string
	homes    func(davProp) *davHrefs
	matches  func(davItem) bool
}

// Resolve a property or response href as advertised, including the exact
// escaping, query, and trailing slash of a principal URL.
func (c *davClient) discoveryURL(from *url.URL, href string) (*url.URL, error) {
	href = strings.TrimSpace(href)
	ref, err := url.Parse(href)
	if err != nil || href == "" || ref.Opaque != "" || ref.User != nil || ref.Fragment != "" {
		return nil, fmt.Errorf("%s advertised an invalid DAV href", c.name)
	}
	u := from.ResolveReference(ref)
	path := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")
	if u.Path != "/" && webdavSegments(path) != nil {
		return nil, fmt.Errorf("%s advertised an invalid DAV path", c.name)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%s advertised an unsupported DAV URL scheme", c.name)
	}
	return u, nil
}

// Translate the public DAV mount back onto the binding. An independently
// advertised sibling on the service origin keeps its path. Always send
// credentials to the configured authority.
func (c *davClient) discoveryTarget(u *url.URL) (*url.URL, error) {
	target := *u
	target.Scheme, target.Host = c.base.Scheme, c.base.Host
	if sameDAVOrigin(u, c.published) {
		bound := strings.TrimSuffix(c.base.EscapedPath(), "/")
		public := strings.TrimSuffix(c.published.EscapedPath(), "/")
		path := u.EscapedPath()
		if prefix, ok := strings.CutSuffix(bound, public); ok {
			// A proxy added a prefix to the whole tree, including a principal
			// above the bound home set.
			path = prefix + path
		} else if _, under := davPathSuffix(public, path); under {
			path = bound + strings.TrimPrefix(path, public)
		}
		target.RawPath = path
		target.Path, _ = url.PathUnescape(path) // assembled from escaped URL paths
		return &target, nil
	}
	if _, under := davPathSuffix(c.base.Path, u.Path); under || sameDAVOrigin(u, c.base) {
		return &target, nil
	}
	return nil, fmt.Errorf("%s advertised a DAV href outside the configured service origin", c.name)
}

func sameDAVOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

func davStatus(line string) int {
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		code, _ := strconv.Atoi(fields[1])
		return code
	}
	return 0
}

func davLookupError(u *url.URL, status int) error {
	return &upstreamError{status: status, err: fmt.Errorf("DAV lookup of %s returned HTTP %d", u.EscapedPath(), status)}
}

func (c *davClient) lookup(ctx context.Context, u *url.URL, query string) (davResponse, error) {
	rows, err := c.requestMultistatus(ctx, "PROPFIND", u, 0, query)
	if err != nil {
		return davResponse{}, fmt.Errorf("DAV lookup of %s: %w", u.EscapedPath(), err)
	}
	if len(rows) != 1 {
		return davResponse{}, fmt.Errorf("%s returned %d resources for a depth-zero DAV lookup of %s", c.name, len(rows), u.EscapedPath())
	}
	if status := davStatus(rows[0].Status); status >= 400 {
		return davResponse{}, davLookupError(u, status)
	}
	return rows[0], nil
}

func (c *davClient) propertyURLs(u *url.URL, row davResponse, property func(davProp) *davHrefs) ([]*url.URL, error) {
	var urls []*url.URL
	for _, ps := range row.Propstat {
		value := property(ps.Prop)
		if value == nil {
			continue
		}
		switch status := davStatus(ps.Status); status {
		case http.StatusNotFound:
			continue // the optional property is absent
		case http.StatusOK:
		default:
			return nil, davLookupError(u, status)
		}
		for _, href := range value.Hrefs {
			target, err := c.discoveryURL(u, href)
			if err != nil {
				return nil, err
			}
			urls = append(urls, target)
		}
	}
	return urls, nil
}

// collections follows the advertised principal and every home set. If
// neither resource names a home, search the binding itself.
func (c *davClient) collections(ctx context.Context, kind davDiscovery) ([]davItem, error) {
	root, err := c.lookup(ctx, c.base, kind.query)
	if err != nil {
		return nil, err
	}
	published, err := c.discoveryURL(c.base, root.Href)
	if err != nil {
		return nil, err
	}
	c.published = published
	self := root.item("")
	if kind.matches(self) {
		if strings.TrimSuffix(published.Path, "/") != strings.TrimSuffix(c.base.Path, "/") {
			self.Key = strings.TrimSuffix(published.Path, "/") + "/"
			self.location = c.base
		}
		return []davItem{self}, nil
	}
	homes, err := c.propertyURLs(published, root, kind.homes)
	if err != nil {
		return nil, err
	}
	if len(homes) == 0 {
		principals, err := c.propertyURLs(published, root, func(p davProp) *davHrefs { return p.CurrentUserPrincipal })
		if err != nil {
			return nil, err
		}
		if len(principals) > 0 {
			u := principals[0]
			if u.String() == published.String() && u.Path != c.base.Path && self.ordinaryFolder() {
				// go-webdav labels the service root's response with the principal
				// href. The principal is a child of that root and still needs its
				// own depth-zero request.
				c.published = c.base
				if _, under := davPathSuffix(c.base.Path, u.Path); !under {
					c.published = u.JoinPath("../")
				}
			}
			target, err := c.discoveryTarget(u)
			if err != nil {
				return nil, err
			}
			if target.String() != c.base.String() {
				principal, err := c.lookup(ctx, target, kind.query)
				if err != nil {
					return nil, err
				}
				homes, err = c.propertyURLs(u, principal, kind.homes)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	if len(homes) == 0 {
		homes = []*url.URL{c.published}
	}
	return c.walkHomes(ctx, kind, homes)
}

const davDiscoveryFolderLimit = 1024

func (c *davClient) walkHomes(ctx context.Context, kind davDiscovery, homes []*url.URL) ([]davItem, error) {
	var collections []davItem
	found := make(map[string]bool)
	visited := make(map[string]bool)
	for len(homes) != 0 {
		u := homes[0]
		homes = homes[1:]
		if visited[u.String()] {
			continue
		}
		if len(visited) >= davDiscoveryFolderLimit {
			return nil, fmt.Errorf("%s exceeded the DAV discovery limit of %d folders", c.name, davDiscoveryFolderLimit)
		}
		visited[u.String()] = true
		requestURL, err := c.discoveryTarget(u)
		if err != nil {
			return nil, err
		}
		rows, err := c.requestMultistatus(ctx, "PROPFIND", requestURL, 1, kind.query)
		if err != nil {
			return nil, fmt.Errorf("listing DAV home or folder %s: %w", u.EscapedPath(), err)
		}
		folder := strings.TrimSuffix(u.Path, "/")
		for _, row := range rows {
			target, err := c.discoveryURL(u, row.Href)
			if err != nil {
				return nil, err
			}
			path := strings.TrimSuffix(target.Path, "/")
			child, under := strings.CutPrefix(path, folder+"/")
			if path != folder && (!under || strings.Contains(child, "/")) {
				continue // Depth: 1 describes only the request target and its children.
			}
			if status := davStatus(row.Status); status >= 400 && path == folder {
				return nil, davLookupError(target, status)
			}
			location, err := c.discoveryTarget(target)
			if err != nil {
				return nil, err
			}
			key := target.Path // public key for a sibling or a reverse-proxied mount
			if relative, ok := davPathSuffix(c.base.Path, target.Path); ok && target.Path == location.Path {
				key = relative
			}
			item := row.item(key)
			if kind.matches(item) {
				if item.Key != "" {
					item.Key = strings.TrimSuffix(item.Key, "/") + "/"
				}
				if !found[item.Key] {
					item.location = location
					collections = append(collections, item)
					found[item.Key] = true
				}
			} else if path != folder && item.ordinaryFolder() {
				homes = append(homes, target)
			}
		}
	}
	return collections, nil
}

func (i davItem) ordinaryFolder() bool {
	return i.collection() && !i.has(func(p davProp) bool {
		t := p.ResourceType
		return t.Calendar != nil || t.AddressBook != nil || t.Principal != nil || t.ScheduleInbox != nil || t.ScheduleOutbox != nil
	})
}

// Resolve absolute console keys against this user's current collections before
// mounting one for REPORT, GET or DELETE. Relative keys use the binding.
func (c *davClient) mountDiscovered(ctx context.Context, kind davDiscovery, key string) error {
	items, err := c.collections(ctx, kind)
	if err != nil {
		return err
	}
	for _, item := range items {
		if !strings.HasPrefix(item.Key, "/") {
			continue
		}
		child, ok := strings.CutPrefix(key, item.Key)
		if ok && !strings.Contains(child, "/") {
			c.base = item.location
			c.keyPrefix = item.Key
			return nil
		}
	}
	return &upstreamError{status: http.StatusNotFound, err: fmt.Errorf("no discovered collection of %s contains that key", c.name)}
}
