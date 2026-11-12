package serviceproxy

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
)

// Console requests use a short-lived, purpose-bound control-plane grant.
// Apps continue to authenticate by their runtime IP without credentials.
const TestRequestHeader = "X-Donkey-Proxy-Test"
const testRequestPurpose = "donkey/proxy-test/v1"

type testGrant struct {
	Space    string `json:"space"`
	Offering string `json:"offering"`
	Method   string `json:"method"`
	URI      string `json:"uri"`
	Expires  int64  `json:"exp"`
}
type testGrantKey struct{}

func SealTestRequest(key []byte, space, offering string, r *http.Request) (string, error) {
	if len(key) != 32 || config.ValidateSpaceName(space) != nil || !config.ValidName(offering) {
		return "", fmt.Errorf("proxy test authorization unavailable")
	}
	return identity.Seal(key, testRequestPurpose, testGrant{Space: space, Offering: offering,
		Method: r.Method, URI: r.URL.RequestURI(), Expires: time.Now().Add(30 * time.Second).Unix()})
}

func handlerWithTests(services config.ProxyServices, access, spaceAccess Access, key []byte) (http.Handler, error) {
	handler, err := Handler(services, func(ctx context.Context, ip, offering string) (bool, error) {
		if grant, ok := ctx.Value(testGrantKey{}).(testGrant); ok {
			if grant.Offering != offering || spaceAccess == nil {
				return false, nil
			}
			return spaceAccess(ctx, grant.Space, offering)
		}
		return access(ctx, ip, offering)
	})
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token := r.Header.Get(TestRequestHeader); token != "" {
			var grant testGrant
			if len(key) != 32 || identity.Unseal(key, testRequestPurpose, token, &grant) != nil ||
				grant.Expires <= time.Now().Unix() || grant.Expires > time.Now().Add(time.Minute).Unix() ||
				grant.Method != r.Method || grant.URI != r.URL.RequestURI() || config.ValidateSpaceName(grant.Space) != nil {
				http.Error(w, "invalid console request authorization", http.StatusForbidden)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), testGrantKey{}, grant))
		}
		handler.ServeHTTP(w, r)
	}), nil
}
