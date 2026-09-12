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
	// Sandbox is the slug of this org's live environment sandbox
	// ({slug}-sandbox), "" when none; SandboxOf points the other way on a
	// sandbox org.
	Sandbox   string `json:"sandbox"`
	SandboxOf string `json:"sandbox_of"`
	// GitHub (gap 14): Continue-with-GitHub mode ("" off, "platform",
	// "custom") and the custom client id; the org's GitHub App for the
	// proxy — id and configured-ness, never the key.
	SocialGithubMode     string `json:"social_github_mode"`
	SocialGithubClientID string `json:"social_github_client_id"`
	GithubAppID          string `json:"github_app_id"`
	GithubAppConfigured  bool   `json:"github_app_configured"`
}

func (c *Client) GetOrg(ctx context.Context) (*Org, error) {
	var o Org
	if err := c.do(ctx, http.MethodGet, "", nil, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// ---- GitHub ----

// SetGithubSignin puts Continue with GitHub on the org's hosted sign-in
// pages: mode "platform" borrows Latchkey's OAuth app, "custom" is the
// org's own (client id + secret — the secret is write-only).
func (c *Client) SetGithubSignin(ctx context.Context, mode, clientID, clientSecret string) error {
	return c.do(ctx, http.MethodPost, "/social/github", map[string]string{
		"mode": mode, "client_id": clientID, "client_secret": clientSecret,
	}, nil)
}

func (c *Client) ClearGithubSignin(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/social/github/clear", map[string]string{}, nil)
}

// SetGithubApp stores the org's GitHub App for the proxy: the numeric app
// id and the private key GitHub generated (write-only; parsed on the
// service side at set time).
func (c *Client) SetGithubApp(ctx context.Context, appID, privateKey string) error {
	return c.do(ctx, http.MethodPost, "/github/app", map[string]string{
		"app_id": appID, "private_key": privateKey,
	}, nil)
}

func (c *Client) ClearGithubApp(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/github/app/clear", map[string]string{}, nil)
}

// CreateSandbox mints the org's environment sandbox — {slug}-sandbox,
// pointing back through sandbox_of, owned by this org's owner. The claim
// converges: minting again is a no-op that still answers the slug.
func (c *Client) CreateSandbox(ctx context.Context) (string, error) {
	var out struct {
		Slug string `json:"slug"`
	}
	if err := c.do(ctx, http.MethodPost, "/sandbox", nil, &out); err != nil {
		return "", err
	}
	return out.Slug, nil
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

// ---- webhooks ----

// Webhook is one signed endpoint as the org API lists it. The secret is
// write-only: the list never echoes it.
type Webhook struct {
	URL    string   `json:"url"`
	Events []string `json:"events"`
}

// SetWebhook registers or re-registers an endpoint. The service keys
// endpoints by (org, url), so a second call with the same url rotates the
// secret / event filter in place and re-activates a disabled endpoint.
func (c *Client) SetWebhook(ctx context.Context, url, secret string, events []string) error {
	if events == nil {
		events = []string{}
	}
	return c.do(ctx, http.MethodPost, "/webhooks", map[string]any{
		"url": url, "secret": secret, "events": events,
	}, nil)
}

// Webhooks lists the org's active endpoints (disabled ones are not
// listed).
func (c *Client) Webhooks(ctx context.Context) ([]Webhook, error) {
	var out struct {
		Items []Webhook `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/webhooks", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetWebhook finds one active endpoint by url; NotFound when it is
// disabled or never registered.
func (c *Client) GetWebhook(ctx context.Context, url string) (*Webhook, error) {
	items, err := c.Webhooks(ctx)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].URL == url {
			return &items[i], nil
		}
	}
	return nil, &APIError{Status: http.StatusNotFound, Message: "no active webhook at " + url}
}

// DisableWebhook stops deliveries to url. The endpoint's history stays;
// a later SetWebhook with the same url re-activates it.
func (c *Client) DisableWebhook(ctx context.Context, url string) error {
	return c.do(ctx, http.MethodPost, "/webhooks/disable", map[string]any{"url": url}, nil)
}

// ---- auth domains ----

type AuthDomain struct {
	Domain string `json:"domain"`
	Issuer string `json:"issuer"`
	// RpId is the passkey scope for the host: the domain itself (server
	// default, reported as "") or a parent of it.
	RpId string `json:"rp_id"`
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

// ClaimAuthDomain claims (or re-claims — the call converges) a domain for
// the org. rpID "" leaves the passkey scope at the domain itself.
func (c *Client) ClaimAuthDomain(ctx context.Context, domain, rpID string) error {
	body := map[string]any{"domain": domain}
	if rpID != "" {
		body["rp_id"] = rpID
	}
	return c.do(ctx, http.MethodPost, "/auth-domains", body, nil)
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

// ---- tenants ----

// Tenant is the org API's tenant object. `Sandbox` (the live sandbox's
// slug) appears only on the single GET, never in the list.
type Tenant struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	Parent      string `json:"parent"`
	SandboxOf   string `json:"sandbox_of"`
	Sandbox     string `json:"sandbox"`
	SsoRequired bool   `json:"sso_required"`
}

func (c *Client) GetTenant(ctx context.Context, slug string) (*Tenant, error) {
	var t Tenant
	if err := c.do(ctx, http.MethodGet, "/tenants/"+url.PathEscape(slug), nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// ClaimTenant creates a tenant. Claim-style: re-claiming an existing
// active tenant converges to a no-op (it does NOT update display name or
// parent — those have their own setters); an archived slug is refused.
func (c *Client) ClaimTenant(ctx context.Context, slug, displayName, parent string) error {
	body := map[string]any{"slug": slug, "display_name": displayName}
	if parent != "" {
		body["parent"] = parent
	}
	return c.do(ctx, http.MethodPost, "/tenants", body, nil)
}

func (c *Client) RenameTenant(ctx context.Context, slug, displayName string) error {
	return c.do(ctx, http.MethodPost, "/tenants/"+url.PathEscape(slug)+"/rename", map[string]any{"display_name": displayName}, nil)
}

func (c *Client) SetTenantParent(ctx context.Context, slug, parent string) error {
	return c.do(ctx, http.MethodPost, "/tenants/"+url.PathEscape(slug)+"/parent", map[string]any{"parent": parent}, nil)
}

func (c *Client) ClearTenantParent(ctx context.Context, slug string) error {
	return c.do(ctx, http.MethodPost, "/tenants/"+url.PathEscape(slug)+"/parent/clear", map[string]any{}, nil)
}

// SetTenantSsoRequired flips the tenant's SSO gate. The wire field is
// `required` on write but reads back as `sso_required`.
func (c *Client) SetTenantSsoRequired(ctx context.Context, slug string, required bool) error {
	return c.do(ctx, http.MethodPost, "/tenants/"+url.PathEscape(slug)+"/sso-required", map[string]any{"required": required}, nil)
}

// ArchiveTenant is the tenant delete — one-way, and the slug can never
// be re-claimed afterwards.
func (c *Client) ArchiveTenant(ctx context.Context, slug string) error {
	return c.do(ctx, http.MethodPost, "/tenants/"+url.PathEscape(slug)+"/archive", map[string]any{}, nil)
}

// ---- role definitions ----

type Role struct {
	Slug         string   `json:"slug"`
	DisplayName  string   `json:"display_name"`
	Capabilities []string `json:"capabilities"`
	BaseLevel    string   `json:"base_level"`
}

// Roles lists the org's live role registry — retired roles never appear.
func (c *Client) Roles(ctx context.Context) ([]Role, error) {
	var out struct {
		Items []Role `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/roles", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetRole finds one role by slug; retired or never-defined answers
// NotFound (the registry list is the only read-back the API offers).
func (c *Client) GetRole(ctx context.Context, slug string) (*Role, error) {
	items, err := c.Roles(ctx)
	if err != nil {
		return nil, err
	}
	for _, role := range items {
		if role.Slug == slug {
			return &role, nil
		}
	}
	return nil, &APIError{Status: http.StatusNotFound, Message: "no role " + slug}
}

// DefineRole creates or fully overwrites a role definition — the
// endpoint is a true upsert, and redefining un-retires.
func (c *Client) DefineRole(ctx context.Context, role Role) error {
	body := map[string]any{"slug": role.Slug, "display_name": role.DisplayName}
	if len(role.Capabilities) > 0 {
		body["capabilities"] = role.Capabilities
	}
	if role.BaseLevel != "" {
		body["base_level"] = role.BaseLevel
	}
	return c.do(ctx, http.MethodPost, "/roles", body, nil)
}

func (c *Client) RetireRole(ctx context.Context, slug string) error {
	return c.do(ctx, http.MethodPost, "/roles/"+url.PathEscape(slug)+"/retire", map[string]any{}, nil)
}

// ---- teams ----

type TeamBinding struct {
	Ns    string   `json:"ns"`
	Level string   `json:"level"`
	Roles []string `json:"roles,omitempty"`
}

type Team struct {
	Slug        string        `json:"slug"`
	DisplayName string        `json:"display_name"`
	Tenant      string        `json:"tenant"`
	IdpManaged  bool          `json:"idp_managed"`
	Members     []string      `json:"members"`
	Bindings    []TeamBinding `json:"bindings"`
}

func (c *Client) GetTeam(ctx context.Context, slug string) (*Team, error) {
	var t Team
	if err := c.do(ctx, http.MethodGet, "/teams/"+url.PathEscape(slug), nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateTeam creates a team under a tenant, or renames it when the slug
// already exists with the same tenant — the endpoint upserts on display
// name only. Moving a team between tenants is refused ("slug is taken"),
// and a deleted team's slug stays retired forever.
func (c *Client) CreateTeam(ctx context.Context, slug, tenant, displayName string) error {
	return c.do(ctx, http.MethodPost, "/teams", map[string]any{
		"slug": slug, "tenant": tenant, "display_name": displayName,
	}, nil)
}

// SetTeamBindings replaces the team's whole binding list; an empty list
// clears every binding.
func (c *Client) SetTeamBindings(ctx context.Context, slug string, bindings []TeamBinding) error {
	if bindings == nil {
		bindings = []TeamBinding{}
	}
	return c.do(ctx, http.MethodPost, "/teams/"+url.PathEscape(slug)+"/bindings", map[string]any{"bindings": bindings}, nil)
}

// AddTeamMember adds by email (the identity is ensured if new) and
// returns the member's root identity id — the only key the team object
// echoes back. Refused on idp_managed teams: the IdP owns those rosters.
func (c *Client) AddTeamMember(ctx context.Context, team, email string) (string, error) {
	var out struct {
		IdentityID string `json:"identity_id"`
	}
	err := c.do(ctx, http.MethodPost, "/teams/"+url.PathEscape(team)+"/members", map[string]any{"email": email}, &out)
	return out.IdentityID, err
}

func (c *Client) RemoveTeamMember(ctx context.Context, team, identityID string) error {
	return c.do(ctx, http.MethodPost, "/teams/"+url.PathEscape(team)+"/members/remove", map[string]any{"identity_id": identityID}, nil)
}

func (c *Client) DeleteTeam(ctx context.Context, slug string) error {
	return c.do(ctx, http.MethodPost, "/teams/"+url.PathEscape(slug)+"/delete", map[string]any{}, nil)
}

// ---- tenant grants ----

// TenantMember is one row of GET /tenants/{tenant}/members — the grant
// read-back. The namespace is implied by the path and never echoed;
// team-conferred access never appears here.
type TenantMember struct {
	IdentityID string   `json:"identity_id"`
	Level      string   `json:"level"`
	Roles      []string `json:"roles"`
	Source     string   `json:"source"`
}

func (c *Client) TenantMembers(ctx context.Context, tenant string) ([]TenantMember, error) {
	var out struct {
		Items []TenantMember `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/tenants/"+url.PathEscape(tenant)+"/members", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GrantNamespace writes a tenant-scoped grant ({org}/{tenant} namespaces
// only — org-level roles stay behind the owner-gated invite flow) and
// returns the root identity id. Convergent upsert: re-granting the same
// shape is a no-op, and a re-grant resets a prior revoke.
func (c *Client) GrantNamespace(ctx context.Context, email, namespace, level string, roles []string) (string, error) {
	body := map[string]any{"email": email, "namespace": namespace, "level": level}
	if len(roles) > 0 {
		body["roles"] = roles
	}
	var out struct {
		IdentityID string `json:"identity_id"`
	}
	err := c.do(ctx, http.MethodPost, "/grants", body, &out)
	return out.IdentityID, err
}

// RevokeNamespaceGrant revokes a tenant-scoped grant. Idempotent — a
// never-granted or already-revoked namespace answers 200.
func (c *Client) RevokeNamespaceGrant(ctx context.Context, email, namespace string) error {
	return c.do(ctx, http.MethodPost, "/grants/revoke", map[string]any{"email": email, "namespace": namespace}, nil)
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
	}
	// sent whenever configured — origins on a secret key must reach the
	// server so its refusal surfaces instead of a silent omission
	if len(allowedOrigins) > 0 {
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
