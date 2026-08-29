// Package latchkey is a minimal client for Latchkey's org API — the
// self-service surface the Terraform provider manages. Authentication is
// OAuth2 client_credentials with the org's own confidential service
// client; the token is minted lazily and refreshed before expiry.
package latchkey

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client speaks to one Latchkey issuer on behalf of one org.
type Client struct {
	Issuer string
	Org    string

	clientID     string
	clientSecret string

	http *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func New(issuer, org, clientID, clientSecret string) *Client {
	return &Client{
		Issuer: strings.TrimSuffix(issuer, "/"), Org: org,
		clientID: clientID, clientSecret: clientSecret,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError carries the org API's error body alongside the status, so
// Terraform diagnostics say what the service said.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("latchkey: %d %s", e.Status, e.Message)
}

// IsNotFound reports a 404 — resources use it to detect drift-deletion.
func IsNotFound(err error) bool {
	var api *APIError
	if ok := asAPIError(err, &api); ok {
		return api.Status == http.StatusNotFound
	}
	return false
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func (c *Client) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Issuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Err         string `json:"error"`
		ErrDesc     string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("token endpoint: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("token endpoint refused: %s %s", out.Err, out.ErrDesc)
	}
	c.token = out.AccessToken
	// refresh a minute early so requests never ride an expiring token
	c.expires = time.Now().Add(time.Duration(out.ExpiresIn)*time.Second - time.Minute)
	return c.token, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	tok, err := c.bearer(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Issuer+"/org/"+c.Org+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// ---- org ----

// Org is the settings snapshot GET /org/{slug} answers. Secrets never
// appear — only configured-ness booleans.
type Org struct {
	Slug            string         `json:"slug"`
	DisplayName     string         `json:"display_name"`
	YourRole        string         `json:"your_role"`
	MailFrom        string         `json:"mail_from"`
	MailFromName    string         `json:"mail_from_name"`
	MailConfigured  bool           `json:"mail_configured"`
	PhoneConfigured bool           `json:"phone_configured"`
	Templates       []MailTemplate `json:"templates"`
	BrandLogoURL    string         `json:"brand_logo_url"`
	BrandAccent     string         `json:"brand_accent"`
	BrandBg         string         `json:"brand_bg"`
	BrandBgFit      string         `json:"brand_bg_fit"`
	BrandBgPosition string         `json:"brand_bg_position"`
	BrandBgScrim    string         `json:"brand_bg_scrim"`
	BrandTagline    string         `json:"brand_tagline"`
}

func (c *Client) GetOrg(ctx context.Context) (*Org, error) {
	var o Org
	if err := c.do(ctx, http.MethodGet, "", nil, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// ---- clients ----

type OidcClient struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Public              bool     `json:"public"`
	Status              string   `json:"status"`
	RedirectUris        []string `json:"redirect_uris"`
	Access              string   `json:"access"`
	PhoneEnabled        bool     `json:"phone_enabled"`
	CaptchaRequired     bool     `json:"captcha_required"`
	AttestationRequired bool     `json:"attestation_required"`
	OtpDailyCeiling     int64    `json:"otp_daily_ceiling"`
}

func (c *Client) Clients(ctx context.Context) ([]OidcClient, error) {
	var out struct {
		Items []OidcClient `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/clients", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetClient finds one client by id; a missing or disabled client answers
// a NotFound error so callers can treat both as gone.
func (c *Client) GetClient(ctx context.Context, id string) (*OidcClient, error) {
	items, err := c.Clients(ctx)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.ID == id && it.Status == "active" {
			return &it, nil
		}
	}
	return nil, &APIError{Status: http.StatusNotFound, Message: "client not found"}
}

func (c *Client) RegisterClient(ctx context.Context, name string, public bool, redirectURIs []string, secretHash string) (string, error) {
	var out struct {
		ClientID string `json:"client_id"`
	}
	err := c.do(ctx, http.MethodPost, "/clients", map[string]any{
		"name": name, "public": public, "redirect_uris": redirectURIs, "secret_hash": secretHash,
	}, &out)
	return out.ClientID, err
}

func (c *Client) SetClientName(ctx context.Context, id, name string) error {
	return c.do(ctx, http.MethodPost, "/clients/name", map[string]any{"client_id": id, "name": name}, nil)
}

func (c *Client) SetClientRedirectURIs(ctx context.Context, id string, uris []string) error {
	return c.do(ctx, http.MethodPost, "/clients/redirect-uris", map[string]any{"client_id": id, "redirect_uris": uris}, nil)
}

func (c *Client) SetClientPhone(ctx context.Context, id string, enabled bool) error {
	return c.do(ctx, http.MethodPost, "/clients/phone", map[string]any{"client_id": id, "phone_enabled": enabled}, nil)
}

func (c *Client) SetClientCaptcha(ctx context.Context, id string, required bool) error {
	return c.do(ctx, http.MethodPost, "/clients/captcha", map[string]any{"client_id": id, "captcha_required": required}, nil)
}

func (c *Client) SetClientAttestation(ctx context.Context, id string, required bool) error {
	return c.do(ctx, http.MethodPost, "/clients/attestation", map[string]any{"client_id": id, "attestation_required": required}, nil)
}

func (c *Client) SetClientOtpCeiling(ctx context.Context, id string, ceiling int64) error {
	return c.do(ctx, http.MethodPost, "/clients/otp-ceiling", map[string]any{"client_id": id, "otp_daily_ceiling": ceiling}, nil)
}

func (c *Client) DisableClient(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/clients/disable", map[string]any{"client_id": id}, nil)
}

// ---- branding ----

type Branding struct {
	LogoURL string `json:"brand_logo_url"`
	Accent  string `json:"brand_accent"`
	Bg      string `json:"brand_bg"`
	Tagline string `json:"brand_tagline"`
}

func (c *Client) SetBranding(ctx context.Context, b Branding) error {
	return c.do(ctx, http.MethodPost, "/branding", b, nil)
}

func (c *Client) ClearBranding(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/branding/clear", map[string]any{}, nil)
}

func (c *Client) SetBackgroundStyle(ctx context.Context, fit, position, scrim string) error {
	return c.do(ctx, http.MethodPost, "/branding/background/style", map[string]any{
		"brand_bg_fit": fit, "brand_bg_position": position, "brand_bg_scrim": scrim,
	}, nil)
}

// ---- mail templates ----

type MailTemplate struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	HTML    string `json:"html"`
}

func (c *Client) SetTemplate(ctx context.Context, t MailTemplate) error {
	return c.do(ctx, http.MethodPost, "/templates", t, nil)
}

func (c *Client) ClearTemplate(ctx context.Context, kind string) error {
	return c.do(ctx, http.MethodPost, "/templates/clear", map[string]any{"kind": kind}, nil)
}

// GetTemplate reads one kind back from the org snapshot; NotFound when
// the org has no custom template of that kind.
func (c *Client) GetTemplate(ctx context.Context, kind string) (*MailTemplate, error) {
	o, err := c.GetOrg(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range o.Templates {
		if t.Kind == kind {
			return &t, nil
		}
	}
	return nil, &APIError{Status: http.StatusNotFound, Message: "no custom template of kind " + kind}
}

// ---- auth domains ----

type AuthDomain struct {
	Domain string `json:"domain"`
	Issuer string `json:"issuer"`
}

func (c *Client) AuthDomains(ctx context.Context) ([]AuthDomain, error) {
	var out struct {
		Items []AuthDomain `json:"auth_domains"`
	}
	if err := c.do(ctx, http.MethodGet, "/auth-domains", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (c *Client) ClaimAuthDomain(ctx context.Context, domain string) error {
	return c.do(ctx, http.MethodPost, "/auth-domains", map[string]any{"domain": domain}, nil)
}

func (c *Client) ReleaseAuthDomain(ctx context.Context, domain string) error {
	return c.do(ctx, http.MethodPost, "/auth-domains/release", map[string]any{"domain": domain}, nil)
}

// ---- membership grants ----

type Member struct {
	ID     string `json:"id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

func (c *Client) Members(ctx context.Context) ([]Member, error) {
	var out struct {
		Items []Member `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/members", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetMember finds a member by email; NotFound when no grant exists.
func (c *Client) GetMember(ctx context.Context, email string) (*Member, error) {
	items, err := c.Members(ctx)
	if err != nil {
		return nil, err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	for _, m := range items {
		if strings.EqualFold(m.Email, email) {
			return &m, nil
		}
	}
	return nil, &APIError{Status: http.StatusNotFound, Message: "no membership for " + email}
}

func (c *Client) Grant(ctx context.Context, email, namespace, role string) error {
	return c.do(ctx, http.MethodPost, "/grants", map[string]any{
		"email": email, "namespace": namespace, "role": role,
	}, nil)
}

func (c *Client) RevokeGrant(ctx context.Context, email, namespace string) error {
	return c.do(ctx, http.MethodPost, "/grants/revoke", map[string]any{
		"email": email, "namespace": namespace,
	}, nil)
}

// ---- tenant API keys ----

// TenantKey is one ledger row — the plaintext never appears here; it
// exists only in the CreateTenantKey response (show-once, hash at rest).
type TenantKey struct {
	Prefix         string   `json:"prefix"`
	Name           string   `json:"name"`
	Scopes         []string `json:"scopes"`
	Browser        bool     `json:"browser"`
	AllowedOrigins []string `json:"allowed_origins"`
	Revoked        bool     `json:"revoked"`
}

// CreateTenantKey mints a key under a tenant. Browser (publishable)
// keys carry the origin allowlist and mint lk_pk_* prefixes; the
// returned plaintext is the only copy that will ever exist.
func (c *Client) CreateTenantKey(ctx context.Context, tenant, name string, scopes []string, browser bool, allowedOrigins []string) (key, prefix string, err error) {
	body := map[string]any{"name": name}
	if len(scopes) > 0 {
		body["scopes"] = scopes
	}
	if browser {
		body["browser"] = true
		body["allowed_origins"] = allowedOrigins
	}
	var out struct {
		Key    string `json:"key"`
		Prefix string `json:"prefix"`
	}
	if err := c.do(ctx, http.MethodPost, "/tenants/"+url.PathEscape(tenant)+"/keys", body, &out); err != nil {
		return "", "", err
	}
	return out.Key, out.Prefix, nil
}

func (c *Client) TenantKeys(ctx context.Context, tenant string) ([]TenantKey, error) {
	var out struct {
		Items []TenantKey `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/tenants/"+url.PathEscape(tenant)+"/keys", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// RevokeTenantKey revokes by display prefix — the server revokes every
// key matching it (prefix collisions revoke together; safe direction).
func (c *Client) RevokeTenantKey(ctx context.Context, tenant, prefix string) error {
	return c.do(ctx, http.MethodPost, "/tenants/"+url.PathEscape(tenant)+"/keys/revoke", map[string]any{
		"prefix": prefix,
	}, nil)
}
