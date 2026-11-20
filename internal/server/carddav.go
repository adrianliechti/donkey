package server

// The contacts console, for services that speak CardDAV: the user's
// address books, the cards in one, and one card read in full. Like the
// WebDAV browser it reaches the service as the caller (davClient); the
// cards are read here (pim) so the console sees people, not vCards.

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/pim"
)

type cardDAV struct {
	dav *davClient
}

// An address book holds at most this many cards for the console: past
// it, the list says it is cut short.
const contactsMax = 5000

// A card read in full is at most this large — a photo included.
const contactBytes = 4 << 20

const cardDAVDiscover = `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:prop>
<D:resourcetype/><D:displayname/><D:current-user-principal/><C:addressbook-home-set/><C:addressbook-description/>
</D:prop></D:propfind>`

// The list asks for what it shows — not the photos, which would make an
// address book's one answer large. A service that sends whole cards anyway
// (Radicale does) is read the same. The filter is empty, so it must test
// "allof": an empty "anyof", the default, matches nothing on Radicale.
const cardDAVQuery = `<?xml version="1.0" encoding="utf-8"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
<D:prop><D:getetag/><C:address-data>
<C:prop name="VERSION"/><C:prop name="UID"/><C:prop name="FN"/><C:prop name="N"/><C:prop name="ORG"/>
<C:prop name="EMAIL"/><C:prop name="TEL"/><C:prop name="KIND"/><C:prop name="X-ADDRESSBOOKSERVER-KIND"/>
</C:address-data></D:prop>
<C:filter test="allof"/>
</C:addressbook-query>`

type addressBook struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type contactSummary struct {
	Key   string `json:"key"`
	ETag  string `json:"etag,omitempty"`
	Name  string `json:"name"`
	Org   string `json:"org,omitempty"`
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
	Kind  string `json:"kind,omitempty"`
}

type contactDetail struct {
	contactSummary
	Names       *pim.Names  `json:"names,omitempty"`
	Nickname    string      `json:"nickname,omitempty"`
	Title       string      `json:"title,omitempty"`
	Role        string      `json:"role,omitempty"`
	Department  string      `json:"department,omitempty"`
	Emails      []pim.Typed `json:"emails"`
	Phones      []pim.Typed `json:"phones"`
	Addresses   []pim.Typed `json:"addresses"`
	URLs        []pim.Typed `json:"urls"`
	Birthday    string      `json:"birthday,omitempty"`
	Anniversary string      `json:"anniversary,omitempty"`
	Note        string      `json:"note,omitempty"`
	Categories  []string    `json:"categories"`
	Photo       string      `json:"photo,omitempty"`
	Members     []string    `json:"members,omitempty"`
	UID         string      `json:"uid,omitempty"`
	Revision    string      `json:"revision,omitempty"`
	Version     string      `json:"version,omitempty"`
	Data        string      `json:"data"`
}

var addressBookDiscovery = davDiscovery{
	protocol: addons.ProtocolCardDAV,
	query:    cardDAVDiscover,
	homes:    func(p davProp) *davHrefs { return p.AddressBookHomeSet },
	matches:  func(i davItem) bool { return i.has(func(p davProp) bool { return p.ResourceType.AddressBook != nil }) },
}

// AddressBooks are the user's, by name.
func (c cardDAV) AddressBooks(ctx context.Context) ([]addressBook, error) {
	items, err := c.dav.collections(ctx, addressBookDiscovery)
	if err != nil {
		return nil, err
	}
	out := []addressBook{}
	for _, item := range items {
		out = append(out, addressBook{
			Key:         item.Key,
			Name:        c.dav.collectionName(item),
			Description: item.prop(func(p davProp) string { return p.AddressBookDescription }),
		})
	}
	slices.SortFunc(out, func(a, b addressBook) int { return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out, nil
}

// Contacts are an address book's cards, by name; truncated says the book
// holds more than contactsMax.
func (c cardDAV) Contacts(ctx context.Context, book string) ([]contactSummary, bool, error) {
	items, err := c.dav.multistatus(ctx, "REPORT", book, 1, cardDAVQuery)
	if err != nil {
		return nil, false, err
	}
	out := []contactSummary{}
	for _, item := range items {
		data := item.prop(func(p davProp) string { return p.AddressData })
		if item.collection() || data == "" {
			continue
		}
		card, ok := pim.ParseContact([]byte(data))
		if !ok {
			continue
		}
		out = append(out, summarize(item.Key, item.prop(func(p davProp) string { return p.ETag }), card))
	}
	// People by name; the nameless last.
	slices.SortFunc(out, func(a, b contactSummary) int {
		if (a.Name == "") != (b.Name == "") {
			if a.Name == "" {
				return 1
			}
			return -1
		}
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.Key, b.Key))
	})
	truncated := len(out) > contactsMax
	if truncated {
		out = out[:contactsMax]
	}
	return out, truncated, nil
}

func summarize(key, etag string, card pim.Contact) contactSummary {
	return contactSummary{Key: key, ETag: etag, Name: card.Name, Org: card.Org, Email: preferred(card.Emails), Phone: preferred(card.Phones), Kind: card.Kind}
}

// preferred is the value marked so, else the first.
func preferred(values []pim.Typed) string {
	for _, v := range values {
		if v.Preferred {
			return v.Value
		}
	}
	if len(values) > 0 {
		return values[0].Value
	}
	return ""
}

// Contact is one card read in full, and as it is stored.
func (c cardDAV) Contact(ctx context.Context, key string) (contactDetail, error) {
	data, etag, err := c.dav.get(ctx, key, contactBytes)
	if err != nil {
		return contactDetail{}, err
	}
	card, ok := pim.ParseContact(data)
	if !ok {
		return contactDetail{}, &upstreamError{status: http.StatusNotFound, err: fmt.Errorf("%s holds no card", key)}
	}
	return contactDetail{
		contactSummary: summarize(key, etag, card),
		Names:          card.Names, Nickname: card.Nickname, Title: card.Title, Role: card.Role, Department: card.Department,
		Emails: nonNil(card.Emails), Phones: nonNil(card.Phones), Addresses: nonNil(card.Addresses), URLs: nonNil(card.URLs),
		Birthday: card.Birthday, Anniversary: card.Anniversary, Note: card.Note, Categories: nonNil(card.Categories),
		Photo: card.Photo, Members: card.Members, UID: card.UID, Revision: card.Revision, Version: card.Version,
		Data: string(data),
	}, nil
}

// Delete removes a card, unless it changed since etag (as the service wrote
// it).
func (c cardDAV) Delete(ctx context.Context, key, etag string) error {
	return c.dav.remove(ctx, key, etag)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ─── Handlers ──────────────────────────────────────────────────────────

func (s *Server) handleAddressBooks(w http.ResponseWriter, r *http.Request) {
	s.serveDAV(w, r, addressBookDiscovery, "", func(ctx context.Context, dav *davClient) (any, error) {
		books, err := cardDAV{dav}.AddressBooks(ctx)
		return map[string]any{"addressBooks": books}, err
	})
}

func (s *Server) handleContacts(w http.ResponseWriter, r *http.Request) {
	book := r.URL.Query().Get("book")
	if err := checkDAVCollection(book); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.serveDAV(w, r, addressBookDiscovery, book, func(ctx context.Context, dav *davClient) (any, error) {
		contacts, truncated, err := cardDAV{dav}.Contacts(ctx, book)
		return map[string]any{"contacts": contacts, "truncated": truncated}, err
	})
}

func (s *Server) handleContact(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if err := checkDAVResource(key); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.serveDAV(w, r, addressBookDiscovery, key, func(ctx context.Context, dav *davClient) (any, error) {
		return cardDAV{dav}.Contact(ctx, key)
	})
}

func (s *Server) handleDeleteContact(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if err := checkDAVResource(key); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.serveDAV(w, r, addressBookDiscovery, key, func(ctx context.Context, dav *davClient) (any, error) {
		return map[string]any{"ok": true}, cardDAV{dav}.Delete(ctx, key, r.URL.Query().Get("etag"))
	})
}
