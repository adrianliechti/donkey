package serviceproxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
)

func TestConsoleGrantIsScopedAndRechecksSpaceBinding(t *testing.T) {
	key := bytes.Repeat([]byte{5}, 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(TestRequestHeader) != "" {
			t.Error("console grant reached the target")
		}
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	granted := true
	handler, err := handlerWithTests(config.ProxyServices{{Name: "erp", TargetURL: upstream.URL}},
		func(context.Context, string, string) (bool, error) { return false, nil },
		func(_ context.Context, space, offering string) (bool, error) {
			return granted && space == "team" && offering == "erp", nil
		}, key)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/services/erp/items?x=1", nil)
	token, err := SealTestRequest(key, "team", "erp", request)
	if err != nil {
		t.Fatal(err)
	}
	expired, _ := identity.Seal(key, testRequestPurpose, testGrant{Space: "team", Offering: "erp", Method: "GET", URI: request.URL.RequestURI(), Expires: time.Now().Add(-time.Minute).Unix()})
	wrongPurpose, _ := identity.Seal(key, "donkey/cli/v1", testGrant{})
	otherSpace, _ := SealTestRequest(key, "other", "erp", request)
	for _, test := range []struct {
		name, method, uri, token string
		status                   int
	}{
		{"valid", "GET", request.URL.RequestURI(), token, 200},
		{"no grant", "GET", request.URL.RequestURI(), "", 403},
		{"tampered", "GET", request.URL.RequestURI(), token + "bad", 403},
		{"expired", "GET", request.URL.RequestURI(), expired, 403},
		{"login credential", "GET", request.URL.RequestURI(), wrongPurpose, 403},
		{"other space", "GET", request.URL.RequestURI(), otherSpace, 403},
		{"method changed", "DELETE", request.URL.RequestURI(), token, 403},
		{"path changed", "GET", "/services/erp/private?x=1", token, 403},
		{"query changed", "GET", "/services/erp/items?x=2", token, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, test.uri, nil)
			r.Header.Set(TestRequestHeader, test.token)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("response = %d %s", w.Code, w.Body.String())
			}
		})
	}
	granted = false
	request.Header.Set(TestRequestHeader, token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != http.StatusForbidden {
		t.Fatal("revoked space binding remained authorized")
	}
}

func TestConsoleGrantRequiresItsOfferingAndKey(t *testing.T) {
	key := bytes.Repeat([]byte{6}, 32)
	r := httptest.NewRequest("GET", "/services/erp/items", nil)
	token, _ := SealTestRequest(key, "team", "other", r)
	r.Header.Set(TestRequestHeader, token)
	for _, testKey := range [][]byte{key, nil, bytes.Repeat([]byte{9}, 32)} {
		h, err := handlerWithTests(config.ProxyServices{{Name: "erp", TargetURL: "https://target.invalid"}}, allowCaller,
			func(context.Context, string, string) (bool, error) { return true, nil }, testKey)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 || strings.Contains(w.Body.String(), token) {
			t.Fatalf("invalid grant accepted or exposed: %d %s", w.Code, w.Body.String())
		}
	}
}
