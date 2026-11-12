package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/latchkeyid/terraform-provider-latchkey/internal/latchkey"
)

// The provider's `token`: a Latchkey access token the caller already
// holds, in place of client_id and client_secret. These tests drive the
// stub's own counter of /oauth/token calls, so they are stub-only.

// tokenStub starts a stub and points the environment at it with no
// credential at all: each test names its own.
func tokenStub(t *testing.T) {
	t.Helper()
	if os.Getenv("LATCHKEY_ISSUER") != "" {
		t.Skip("drives the stub's /oauth/token counter")
	}
	srv := newStub("acme")
	t.Cleanup(srv.Close)
	t.Setenv("LATCHKEY_ISSUER", srv.URL)
	t.Setenv("LATCHKEY_ORG", "acme")
	t.Setenv("LATCHKEY_CLIENT_ID", "")
	t.Setenv("LATCHKEY_CLIENT_SECRET", "")
	t.Setenv("LATCHKEY_TOKEN", "")
	t.Setenv("TF_ACC", "1")
}

// stubTokenRequests is how many times anything signed in at the stub.
func stubTokenRequests() (int, error) {
	resp, err := http.Get(os.Getenv("LATCHKEY_ISSUER") + "/stub/token-requests")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Count int `json:"count"`
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out.Count, err
}

func noTokenRequests(*terraform.State) error {
	n, err := stubTokenRequests()
	if err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("the provider called /oauth/token %d time(s) though it was handed a token", n)
	}
	return nil
}

// spaced matches msg however Terraform wrapped it.
func spaced(msg string) *regexp.Regexp {
	return regexp.MustCompile(strings.Join(strings.Fields(regexp.QuoteMeta(msg)), `\s+`))
}

const tokenTenant = `
resource "latchkey_tenant" "shop" {
  slug         = "token-shop"
  display_name = "Token Shop"
}
`

// a token in the provider block is the whole credential: the resource is
// created, read and destroyed with it, and nothing signs in
func TestAccProviderToken(t *testing.T) {
	tokenStub(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             noTokenRequests,
		Steps: []resource.TestStep{{
			Config: `
provider "latchkey" {
  token = "stub-token"
}
` + tokenTenant,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("latchkey_tenant.shop", "display_name", "Token Shop"),
				noTokenRequests,
			),
		}},
	})
}

// LATCHKEY_TOKEN is the same credential from the environment
func TestAccProviderTokenEnv(t *testing.T) {
	tokenStub(t)
	t.Setenv("LATCHKEY_TOKEN", "stub-token")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             noTokenRequests,
		Steps: []resource.TestStep{{
			Config: tokenTenant,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("latchkey_tenant.shop", "slug", "token-shop"),
				noTokenRequests,
			),
		}},
	})
}

// without a token nothing changed: the client signs in at /oauth/token
// (which also shows the counter counts)
func TestAccProviderClientCredentialsStillSignIn(t *testing.T) {
	tokenStub(t)
	t.Setenv("LATCHKEY_CLIENT_ID", "00000000-0000-0000-0000-000000000001")
	t.Setenv("LATCHKEY_CLIENT_SECRET", "stub-secret")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config: tokenTenant,
			Check: func(*terraform.State) error {
				if n, err := stubTokenRequests(); err != nil || n == 0 {
					return fmt.Errorf("a client_credentials provider made %d /oauth/token call(s): %v", n, err)
				}
				return nil
			},
		}},
	})
}

// a token and a client are two credentials: refused, naming both, from
// the block or from the environment
func TestAccProviderTokenWithClientRefused(t *testing.T) {
	tokenStub(t)
	refused := spaced("token is exclusive with client_id and client_secret")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "latchkey" {
  token         = "stub-token"
  client_secret = "stub-secret"
}
` + tokenTenant,
				ExpectError: refused,
			},
			{
				Config: `
provider "latchkey" {
  token     = "stub-token"
  client_id = "00000000-0000-0000-0000-000000000001"
}
` + tokenTenant,
				ExpectError: refused,
			},
			{
				PreConfig: func() { os.Setenv("LATCHKEY_CLIENT_SECRET", "stub-secret") },
				Config: `
provider "latchkey" {
  token = "stub-token"
}
` + tokenTenant,
				ExpectError: refused,
			},
		},
	})
	if n, err := stubTokenRequests(); err != nil || n != 0 {
		t.Fatalf("a refused configuration still signed in: %d call(s), %v", n, err)
	}
}

// neither a token nor a secret: today's message, word for word
func TestAccProviderNoCredentialRefused(t *testing.T) {
	tokenStub(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "latchkey" {
  client_id = "00000000-0000-0000-0000-000000000001"
}
` + tokenTenant,
				ExpectError: spaced("client_secret is required (set it in the provider block or via LATCHKEY_CLIENT_SECRET)"),
			},
			{
				Config:      tokenTenant,
				ExpectError: spaced("client_id is required (set it in the provider block or via LATCHKEY_CLIENT_ID)"),
			},
		},
	})
}

// the token is a secret: sensitive in the schema, and never in an error —
// not the API's refusal of it, nor a failure to reach the issuer
func TestProviderTokenIsNeverShown(t *testing.T) {
	var resp fwprovider.SchemaResponse
	New("test")().Schema(context.Background(), fwprovider.SchemaRequest{}, &resp)
	if !resp.Schema.Attributes["token"].IsSensitive() {
		t.Fatal("the provider's token must be sensitive")
	}

	const token = "lk-very-secret-token-value"
	var sawBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			t.Errorf("a token client called /oauth/token")
		}
		sawBearer = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusForbidden)
		// even a server that echoes nothing useful
		json.NewEncoder(w).Encode(map[string]string{"error": "sign in first"})
	}))
	api := latchkey.NewWithToken(srv.URL, "acme", token)
	_, err := api.GetOrg(context.Background())
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("the API's refusal: %v", err)
	}
	if sawBearer != "Bearer "+token {
		t.Fatalf("the token was not sent as it is: %q", sawBearer)
	}
	srv.Close()
	if _, err := api.GetOrg(context.Background()); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("an unreachable issuer: %v", err)
	}
}
