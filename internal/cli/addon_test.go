package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/apiclient"
)

func TestTunnelBindingPreservesCredentialsAndOtherDestinations(t *testing.T) {
	for _, tc := range []struct{ key, value, host, want string }{
		{"MYSQL_PASSWORD", "db:3306", "db", "db:3306"},
		{"MYSQL_PASSWORD", "3306", "db", "3306"},
		{"MYSQL_USER", "db", "db", "db"},
		{"MYSQL_HOST", "db", "db", "localhost"},
		{"MYSQL_PORT", "3306", "db", "13306"},
		{"MYSQL_PORT", "5432", "db", "5432"},
		{"DATABASE_URL", "mysql://db:db@db:3306/db?label=db:3306", "db", "mysql://db:db@localhost:13306/db?label=db:3306"},
		{"DATABASE_URL", "mysql://u:p%40ss@[2001:db8::1]:3306/app", "2001:db8::1", "mysql://u:p%40ss@localhost:13306/app"},
		{"DATABASE_URL", "mysql://db:db@other:3306/db", "db", "mysql://db:db@other:3306/db"},
		{"DATABASE_URL", "mysql://u:p@db:3307/app", "db", "mysql://u:p@db:3307/app"},
		{"JDBC_DATABASE_URL", "jdbc:mysql://db/app", "db", "jdbc:mysql://localhost:13306/app"},
		{"REDIS_ADDR", "[::1]:3306", "::1", "localhost:13306"},
	} {
		if got := localizeBinding(tc.key, tc.value, tc.host, 3306, 13306); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.key, got, tc.want)
		}
	}
}

// A local client talks to an HTTP tunnel as if it were the service (here
// Trino): requests reach the service's tunnel route with the CLI's credential
// instead of the client's, and link fields come back pointed at the tunnel,
// with everything else untouched.
func TestHTTPTunnelProxiesAndRebasesLinks(t *testing.T) {
	const remote = "http://trino.internal:8080/engine"
	donkey := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/spaces/default/services/warehouse/tunnel/v1/statement" || r.URL.RawQuery != "x=1" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.RequestURI())
		}
		if r.Header.Get("Authorization") != "Bearer cli-token" || r.Header.Get("Cookie") != "" {
			t.Errorf("credentials: %q %q", r.Header.Get("Authorization"), r.Header.Get("Cookie"))
		}
		if r.Header.Get(addons.TunnelAuthorizationHeader) != "Bearer client-supplied" {
			t.Error("the client's own Authorization should ride beside donkey's")
		}
		if r.Header.Get("X-Trino-Catalog") != "hive" {
			t.Error("dropped the client's session header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"q1","infoUri":"http://trino.internal:8080/ui/query.html?q1",` +
			`"nextUri":"` + remote + `/v1/statement/executing/q1/x/1",` +
			`"partialCancelUri":"` + remote + `/v1/statement/executing/partialCancel/q1/0/x/1",` +
			`"data":[[9007199254740993,"` + remote + `/v1/statement/in-a-row"]]}`))
	}))
	defer donkey.Close()
	api, err := apiclient.New(donkey.URL, "cli-token", "")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := api.TunnelProxy("default", "warehouse")
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(proxy)
	defer local.Close()
	links := addons.TunnelFor("", addons.ProtocolTrino).Links
	proxy.ModifyResponse = func(res *http.Response) error { return rebaseLinks(res, links, remote, local.URL) }

	req, _ := http.NewRequest(http.MethodPost, local.URL+"/v1/statement?x=1", strings.NewReader("SELECT 1"))
	req.Header.Set("Authorization", "Bearer client-supplied")
	req.Header.Set("Cookie", "local=1")
	req.Header.Set("X-Trino-Catalog", "hive")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, want := range []string{
		`"nextUri":"` + local.URL + `/v1/statement/executing/q1/x/1"`,
		`"partialCancelUri":"` + local.URL + `/v1/statement/executing/partialCancel/q1/0/x/1"`,
		`"infoUri":"http://trino.internal:8080/ui/query.html?q1"`,
		`[9007199254740993,"` + remote + `/v1/statement/in-a-row"]`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
}

// A WebDAV client mounting the tunnel walks the tree by the hrefs the
// service answers and follows the Location of what it makes: both come back
// pointed at the tunnel, whether the service wrote them as URLs or as paths
// under its own base. Links elsewhere, and everything else, pass untouched.
func TestHTTPTunnelRebasesLocationAndMultistatusHrefs(t *testing.T) {
	const remote = "http://files.internal:8080/remote/dav"
	donkey := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "MKCOL":
			w.Header().Set("Location", remote+"/new%20folder/")
			w.WriteHeader(http.StatusCreated)
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(http.StatusMultiStatus)
			w.Write([]byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">` +
				`<D:response><D:href>/remote/dav/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop></D:propstat></D:response>` +
				`<D:response><d:href xmlns:d="DAV:">/remote/dav/e%20f.txt</d:href></D:response>` +
				`<D:response><D:href>` + remote + `/sub/</D:href><D:propstat><D:prop><x:href xmlns:x="urn:other">/remote/davenport</x:href><D:owner><D:href>mailto:someone@example.com</D:href></D:owner></D:prop></D:propstat></D:response>` +
				`</D:multistatus>`))
		default:
			t.Errorf("unexpected %s", r.Method)
		}
	}))
	defer donkey.Close()
	api, err := apiclient.New(donkey.URL, "cli-token", "")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := api.TunnelProxy("default", "store")
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(proxy)
	defer local.Close()
	proxy.ModifyResponse = tunnelResponse(addons.TunnelFor("", addons.ProtocolWebDAV).Links, remote, local.URL)

	req, _ := http.NewRequest("MKCOL", local.URL+"/new%20folder/", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 201 || res.Header.Get("Location") != local.URL+"/new%20folder/" {
		t.Fatalf("MKCOL: %d Location %q", res.StatusCode, res.Header.Get("Location"))
	}
	req, _ = http.NewRequest("PROPFIND", local.URL+"/", nil)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 207 || res.ContentLength != int64(len(body)) {
		t.Fatalf("PROPFIND: %d, %d bytes announced, %d sent", res.StatusCode, res.ContentLength, len(body))
	}
	for _, want := range []string{
		`<D:href>/</D:href>`,
		`<d:href xmlns:d="DAV:">/e%20f.txt</d:href>`,
		`<D:href>` + local.URL + `/sub/</D:href>`,
		`<x:href xmlns:x="urn:other">/remote/davenport</x:href>`,
		`<D:href>mailto:someone@example.com</D:href>`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
}
