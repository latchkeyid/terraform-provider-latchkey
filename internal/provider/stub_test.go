package provider

// A faithful in-memory stub of Latchkey's org API, so acceptance tests
// drive the real terraform CLI against real HTTP without a database.
// Point LATCHKEY_ISSUER at a live instance instead to run the same tests
// against the real service.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/template"

	"github.com/google/uuid"
)

type stubClient struct {
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

type stubTemplate struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	HTML    string `json:"html"`
}

type stubKey struct {
	Prefix         string   `json:"prefix"`
	Name           string   `json:"name"`
	Scopes         []string `json:"scopes"`
	Browser        bool     `json:"browser"`
	AllowedOrigins []string `json:"allowed_origins"`
	Revoked        bool     `json:"revoked"`
}

type stubTenant struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	Parent      string `json:"parent,omitempty"`
	SandboxOf   string `json:"sandbox_of,omitempty"`
	SsoRequired bool   `json:"sso_required"`
}

type stubRole struct {
	Slug         string   `json:"slug"`
	DisplayName  string   `json:"display_name"`
	Capabilities []string `json:"capabilities"`
	BaseLevel    string   `json:"base_level"`
	Retired      bool     `json:"-"`
}

type stubBinding struct {
	Ns    string   `json:"ns"`
	Level string   `json:"level"`
	Roles []string `json:"roles"`
}

type stubTeam struct {
	Slug        string        `json:"slug"`
	DisplayName string        `json:"display_name"`
	Tenant      string        `json:"tenant"`
	IdpManaged  bool          `json:"idp_managed"`
	Deleted     bool          `json:"-"`
	Members     []string      `json:"members"`
	Bindings    []stubBinding `json:"bindings"`
}

type stubTGrant struct {
	IdentityID string
	Level      string
	Roles      []string
	Revoked    bool
}

type stubState struct {
	mu         sync.Mutex
	clients    map[string]*stubClient
	templates  map[string]stubTemplate
	domains    map[string]bool
	members    map[string]string // email → role
	branding   map[string]string
	bgStyle    map[string]string
	keys       map[string][]*stubKey // tenant → ledger
	keySeq     int
	tenants    map[string]*stubTenant
	roles      map[string]*stubRole
	teams      map[string]*stubTeam
	identities map[string]string       // email → identity id
	tgrants    map[string]*stubTGrant  // identity id + "|" + ns
	webhooks   map[string]*stubWebhook // url
}

type stubWebhook struct {
	URL    string
	Secret string
	Events []string
	Active bool
}

// tenant/team/role slugs share one normalization rule with the live API
var stubSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func stubSlug(v string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	return v, stubSlugRe.MatchString(v)
}

func newStub(org string) *httptest.Server {
	s := &stubState{
		clients:    map[string]*stubClient{},
		templates:  map[string]stubTemplate{},
		domains:    map[string]bool{},
		members:    map[string]string{},
		branding:   map[string]string{},
		bgStyle:    map[string]string{"brand_bg_fit": "", "brand_bg_position": "", "brand_bg_scrim": ""},
		keys:       map[string][]*stubKey{},
		tenants:    map[string]*stubTenant{},
		roles:      map[string]*stubRole{},
		teams:      map[string]*stubTeam{},
		identities: map[string]string{},
		tgrants:    map[string]*stubTGrant{},
		webhooks:   map[string]*stubWebhook{},
	}
	mux := http.NewServeMux()
	prefix := "/org/" + org

	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostFormValue("grant_type") != "client_credentials" || r.PostFormValue("client_secret") == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "stub-token", "token_type": "Bearer", "expires_in": 3600})
	})

	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer stub-token" {
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]string{"error": "sign in first"})
				return
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			h(w, r)
		}
	}
	body := func(r *http.Request) map[string]any {
		out := map[string]any{}
		json.NewDecoder(r.Body).Decode(&out)
		return out
	}
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	strs := func(m map[string]any, k string) []string {
		out := []string{}
		if raw, ok := m[k].([]any); ok {
			for _, v := range raw {
				out = append(out, fmt.Sprint(v))
			}
		}
		return out
	}
	oops := func(w http.ResponseWriter, code int, msg string) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	// ensure-identity by email, the org API's convergent shape
	identity := func(email string) string {
		email = strings.ToLower(strings.TrimSpace(email))
		if id, ok := s.identities[email]; ok {
			return id
		}
		id := uuid.NewString()
		s.identities[email] = id
		return id
	}
	liveTeam := func(slug string) *stubTeam {
		t, ok := s.teams[slug]
		if !ok || t.Deleted {
			return nil
		}
		return t
	}

	mux.HandleFunc("GET "+prefix, authed(func(w http.ResponseWriter, r *http.Request) {
		templates := []stubTemplate{}
		for _, t := range s.templates {
			templates = append(templates, t)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"slug": org, "display_name": "Stub Org", "your_role": "owner",
			"mail_configured": true, "phone_configured": false,
			"templates":         templates,
			"brand_logo_url":    s.branding["brand_logo_url"],
			"brand_accent":      s.branding["brand_accent"],
			"brand_bg":          s.branding["brand_bg"],
			"brand_tagline":     s.branding["brand_tagline"],
			"brand_bg_fit":      s.bgStyle["brand_bg_fit"],
			"brand_bg_position": s.bgStyle["brand_bg_position"],
			"brand_bg_scrim":    s.bgStyle["brand_bg_scrim"],
		})
	}))

	mux.HandleFunc("POST "+prefix+"/clients", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		id := uuid.NewString()
		uris := []string{}
		if raw, ok := b["redirect_uris"].([]any); ok {
			for _, u := range raw {
				uris = append(uris, fmt.Sprint(u))
			}
		}
		public, _ := b["public"].(bool)
		s.clients[id] = &stubClient{ID: id, Name: str(b, "name"), Public: public, Status: "active", RedirectUris: uris, Access: "rw"}
		json.NewEncoder(w).Encode(map[string]string{"client_id": id})
	}))
	mux.HandleFunc("GET "+prefix+"/clients", authed(func(w http.ResponseWriter, r *http.Request) {
		items := []stubClient{}
		for _, c := range s.clients {
			items = append(items, *c)
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	clientSetter := func(apply func(c *stubClient, b map[string]any)) http.HandlerFunc {
		return authed(func(w http.ResponseWriter, r *http.Request) {
			b := body(r)
			c, ok := s.clients[str(b, "client_id")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]string{"error": "not this organization's client"})
				return
			}
			apply(c, b)
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		})
	}
	mux.HandleFunc("POST "+prefix+"/clients/name", clientSetter(func(c *stubClient, b map[string]any) { c.Name = str(b, "name") }))
	mux.HandleFunc("POST "+prefix+"/clients/disable", clientSetter(func(c *stubClient, b map[string]any) { c.Status = "disabled" }))
	mux.HandleFunc("POST "+prefix+"/clients/phone", clientSetter(func(c *stubClient, b map[string]any) { c.PhoneEnabled, _ = b["phone_enabled"].(bool) }))
	mux.HandleFunc("POST "+prefix+"/clients/captcha", clientSetter(func(c *stubClient, b map[string]any) { c.CaptchaRequired, _ = b["captcha_required"].(bool) }))
	mux.HandleFunc("POST "+prefix+"/clients/attestation", clientSetter(func(c *stubClient, b map[string]any) { c.AttestationRequired, _ = b["attestation_required"].(bool) }))
	mux.HandleFunc("POST "+prefix+"/clients/otp-ceiling", clientSetter(func(c *stubClient, b map[string]any) {
		v, _ := b["otp_daily_ceiling"].(float64)
		c.OtpDailyCeiling = int64(v)
	}))
	mux.HandleFunc("POST "+prefix+"/clients/redirect-uris", clientSetter(func(c *stubClient, b map[string]any) {
		uris := []string{}
		if raw, ok := b["redirect_uris"].([]any); ok {
			for _, u := range raw {
				uris = append(uris, fmt.Sprint(u))
			}
		}
		c.RedirectUris = uris
	}))

	// tenant API keys: mirrors the live surface's rules — browser keys
	// need origins, secret keys refuse them, mint returns key + prefix
	mux.HandleFunc("POST "+prefix+"/tenants/{tenant}/keys", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		browser, _ := b["browser"].(bool)
		origins := []string{}
		if raw, ok := b["allowed_origins"].([]any); ok {
			for _, o := range raw {
				origins = append(origins, fmt.Sprint(o))
			}
		}
		if browser && len(origins) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "a browser key needs at least one allowed origin"})
			return
		}
		if !browser && len(origins) > 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "allowed origins are for browser keys"})
			return
		}
		scopes := []string{}
		if raw, ok := b["scopes"].([]any); ok {
			for _, sc := range raw {
				scopes = append(scopes, fmt.Sprint(sc))
			}
		}
		s.keySeq++
		marker := "lk_live_"
		if browser {
			marker = "lk_pk_live_"
		}
		// the unique part leads so display prefixes (marker+4) never
		// collide — a colliding stub would hide rotation bugs
		key := fmt.Sprintf("%s%02dABrandomrandomrandomrandomCHKSUM", marker, s.keySeq)
		tenant := r.PathValue("tenant")
		row := &stubKey{
			Prefix: key[:len(marker)+4], Name: str(b, "name"),
			Scopes: scopes, Browser: browser, AllowedOrigins: origins,
		}
		s.keys[tenant] = append(s.keys[tenant], row)
		json.NewEncoder(w).Encode(map[string]any{"status": "created", "key": key, "prefix": row.Prefix})
	}))
	mux.HandleFunc("GET "+prefix+"/tenants/{tenant}/keys", authed(func(w http.ResponseWriter, r *http.Request) {
		items := []stubKey{}
		for _, k := range s.keys[r.PathValue("tenant")] {
			items = append(items, *k)
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	mux.HandleFunc("POST "+prefix+"/tenants/{tenant}/keys/revoke", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		found := false
		for _, k := range s.keys[r.PathValue("tenant")] {
			if k.Prefix == str(b, "prefix") {
				k.Revoked, found = true, true
			}
		}
		if !found {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "unknown key"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
	}))

	mux.HandleFunc("POST "+prefix+"/branding", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		s.branding = map[string]string{
			"brand_logo_url": str(b, "brand_logo_url"), "brand_accent": str(b, "brand_accent"),
			"brand_bg": str(b, "brand_bg"), "brand_tagline": str(b, "brand_tagline"),
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "saved"})
	}))
	mux.HandleFunc("POST "+prefix+"/branding/clear", authed(func(w http.ResponseWriter, r *http.Request) {
		s.branding = map[string]string{}
		json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
	}))
	mux.HandleFunc("POST "+prefix+"/branding/background/style", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		s.bgStyle = map[string]string{
			"brand_bg_fit": str(b, "brand_bg_fit"), "brand_bg_position": str(b, "brand_bg_position"),
			"brand_bg_scrim": str(b, "brand_bg_scrim"),
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "saved"})
	}))

	mux.HandleFunc("POST "+prefix+"/templates", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		kind := str(b, "kind")
		if err := stubValidateTemplate(kind, str(b, "subject"), str(b, "body"), str(b, "html")); err != nil {
			// the live API answers userError(err): the text after the
			// last ": " — for a bad placeholder that is the "available"
			// list, which is what a terraform apply shows
			msg := err.Error()
			if i := strings.LastIndex(msg, ": "); i >= 0 {
				msg = msg[i+2:]
			}
			oops(w, http.StatusBadRequest, msg)
			return
		}
		s.templates[kind] = stubTemplate{Kind: kind, Subject: str(b, "subject"), Body: str(b, "body"), HTML: str(b, "html")}
		json.NewEncoder(w).Encode(map[string]string{"status": "saved"})
	}))
	mux.HandleFunc("POST "+prefix+"/templates/clear", authed(func(w http.ResponseWriter, r *http.Request) {
		delete(s.templates, str(body(r), "kind"))
		json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
	}))

	// ---- webhooks: endpoints keyed by (org, url); the list shows only
	// active ones plus the event catalog, exactly like the live API ----
	stubWebhookEvents := []string{
		"tenant.created", "tenant.renamed", "tenant.reparented", "tenant.archived",
		"membership.set", "membership.revoked", "team.updated",
		"invitation.created", "invitation.resent", "invitation.accepted",
		"invitation.declined", "invitation.revoked", "invitation.expired",
	}
	mux.HandleFunc("GET "+prefix+"/webhooks", authed(func(w http.ResponseWriter, r *http.Request) {
		items := []map[string]any{}
		urls := make([]string, 0, len(s.webhooks))
		for u := range s.webhooks {
			urls = append(urls, u)
		}
		sort.Strings(urls)
		for _, u := range urls {
			if wh := s.webhooks[u]; wh.Active {
				items = append(items, map[string]any{"url": wh.URL, "events": wh.Events})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items, "event_types": stubWebhookEvents})
	}))
	mux.HandleFunc("POST "+prefix+"/webhooks", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		u := str(b, "url")
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			oops(w, http.StatusBadRequest, "url: must be an absolute http(s) URL")
			return
		}
		if str(b, "secret") == "" {
			oops(w, http.StatusBadRequest, "secret required")
			return
		}
		s.webhooks[u] = &stubWebhook{URL: u, Secret: str(b, "secret"), Events: strs(b, "events"), Active: true}
		json.NewEncoder(w).Encode(map[string]string{"status": "set"})
	}))
	mux.HandleFunc("POST "+prefix+"/webhooks/disable", authed(func(w http.ResponseWriter, r *http.Request) {
		wh, ok := s.webhooks[str(body(r), "url")]
		if !ok {
			oops(w, http.StatusNotFound, "unknown webhook")
			return
		}
		wh.Active = false
		json.NewEncoder(w).Encode(map[string]string{"status": "disabled"})
	}))

	mux.HandleFunc("GET "+prefix+"/auth-domains", authed(func(w http.ResponseWriter, r *http.Request) {
		items := []map[string]string{}
		for d := range s.domains {
			items = append(items, map[string]string{"domain": d, "issuer": "https://" + d})
		}
		json.NewEncoder(w).Encode(map[string]any{"auth_domains": items})
	}))
	mux.HandleFunc("POST "+prefix+"/auth-domains", authed(func(w http.ResponseWriter, r *http.Request) {
		d := strings.ToLower(str(body(r), "domain"))
		s.domains[d] = true
		json.NewEncoder(w).Encode(map[string]string{"domain": d})
	}))
	mux.HandleFunc("POST "+prefix+"/auth-domains/release", authed(func(w http.ResponseWriter, r *http.Request) {
		delete(s.domains, strings.ToLower(str(body(r), "domain")))
		json.NewEncoder(w).Encode(map[string]string{"status": "released"})
	}))

	// tenants: claim-style creation (converges on an existing active
	// tenant, refuses an archived slug), config through dedicated setters
	mux.HandleFunc("POST "+prefix+"/tenants", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		slug, ok := stubSlug(str(b, "slug"))
		if !ok {
			oops(w, http.StatusBadRequest, fmt.Sprintf("%q is not a usable tenant slug", str(b, "slug")))
			return
		}
		if strings.TrimSpace(str(b, "display_name")) == "" {
			oops(w, http.StatusBadRequest, "display_name is required")
			return
		}
		parent := ""
		if raw := strings.TrimSpace(str(b, "parent")); raw != "" {
			p, ok := stubSlug(raw)
			if !ok {
				oops(w, http.StatusBadRequest, fmt.Sprintf("parent: %q is not a usable tenant slug", raw))
				return
			}
			pt, exists := s.tenants[p]
			switch {
			case !exists || pt.Status != "active":
				oops(w, http.StatusBadRequest, "parent tenant does not exist")
				return
			case pt.Parent != "":
				oops(w, http.StatusBadRequest, "parent tenant is not a root — tenants nest at most one level")
				return
			case pt.SandboxOf != "":
				oops(w, http.StatusBadRequest, "a sandbox cannot be an enterprise")
				return
			}
			parent = p
		}
		if t, exists := s.tenants[slug]; exists {
			if t.Status == "archived" {
				oops(w, http.StatusBadRequest, fmt.Sprintf("tenant %q is archived", slug))
				return
			}
			// converge: an existing active tenant is a no-op, not an update
			json.NewEncoder(w).Encode(map[string]string{"status": "claimed", "slug": slug})
			return
		}
		s.tenants[slug] = &stubTenant{Slug: slug, DisplayName: str(b, "display_name"), Status: "active", Parent: parent}
		json.NewEncoder(w).Encode(map[string]string{"status": "claimed", "slug": slug})
	}))
	mux.HandleFunc("GET "+prefix+"/tenants/{tenant}", authed(func(w http.ResponseWriter, r *http.Request) {
		slug, _ := stubSlug(r.PathValue("tenant"))
		t, ok := s.tenants[slug]
		if !ok {
			oops(w, http.StatusNotFound, "unknown tenant")
			return
		}
		json.NewEncoder(w).Encode(t)
	}))
	tenantSetter := func(apply func(t *stubTenant, b map[string]any) (int, string)) http.HandlerFunc {
		return authed(func(w http.ResponseWriter, r *http.Request) {
			slug, _ := stubSlug(r.PathValue("tenant"))
			t, ok := s.tenants[slug]
			if !ok {
				oops(w, http.StatusNotFound, "unknown tenant")
				return
			}
			b := body(r)
			if code, msg := apply(t, b); code != 0 {
				oops(w, code, msg)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		})
	}
	archivedGuard := func(t *stubTenant) (int, string) {
		if t.Status == "archived" {
			return http.StatusBadRequest, fmt.Sprintf("tenant %q is archived", t.Slug)
		}
		return 0, ""
	}
	mux.HandleFunc("POST "+prefix+"/tenants/{tenant}/rename", tenantSetter(func(t *stubTenant, b map[string]any) (int, string) {
		if code, msg := archivedGuard(t); code != 0 {
			return code, msg
		}
		if strings.TrimSpace(str(b, "display_name")) == "" {
			return http.StatusBadRequest, "display_name is required"
		}
		t.DisplayName = str(b, "display_name")
		return 0, ""
	}))
	mux.HandleFunc("POST "+prefix+"/tenants/{tenant}/parent", tenantSetter(func(t *stubTenant, b map[string]any) (int, string) {
		if code, msg := archivedGuard(t); code != 0 {
			return code, msg
		}
		if t.SandboxOf != "" {
			return http.StatusBadRequest, fmt.Sprintf("a sandbox keeps its prod tenant's grouping — reparent %q instead", t.SandboxOf)
		}
		p, ok := stubSlug(str(b, "parent"))
		if !ok {
			return http.StatusBadRequest, "parent tenant does not exist"
		}
		pt, exists := s.tenants[p]
		switch {
		case !exists || pt.Status != "active":
			return http.StatusBadRequest, "parent tenant does not exist"
		case pt.Parent != "":
			return http.StatusBadRequest, "parent tenant is not a root — tenants nest at most one level"
		}
		for _, other := range s.tenants {
			if other.Parent == t.Slug {
				return http.StatusBadRequest, "tenant has child tenants — an enterprise cannot itself join one"
			}
		}
		t.Parent = p
		return 0, ""
	}))
	mux.HandleFunc("POST "+prefix+"/tenants/{tenant}/parent/clear", tenantSetter(func(t *stubTenant, _ map[string]any) (int, string) {
		if code, msg := archivedGuard(t); code != 0 {
			return code, msg
		}
		t.Parent = ""
		return 0, ""
	}))
	mux.HandleFunc("POST "+prefix+"/tenants/{tenant}/sso-required", tenantSetter(func(t *stubTenant, b map[string]any) (int, string) {
		if code, msg := archivedGuard(t); code != 0 {
			return code, msg
		}
		t.SsoRequired, _ = b["required"].(bool)
		return 0, ""
	}))
	mux.HandleFunc("POST "+prefix+"/tenants/{tenant}/archive", tenantSetter(func(t *stubTenant, _ map[string]any) (int, string) {
		t.Status = "archived"
		return 0, ""
	}))
	mux.HandleFunc("GET "+prefix+"/tenants/{tenant}/members", authed(func(w http.ResponseWriter, r *http.Request) {
		slug, _ := stubSlug(r.PathValue("tenant"))
		if _, ok := s.tenants[slug]; !ok {
			oops(w, http.StatusNotFound, "unknown tenant")
			return
		}
		ns := org + "/" + slug
		items := []map[string]any{}
		for key, g := range s.tgrants {
			if g.Revoked || !strings.HasSuffix(key, "|"+ns) {
				continue
			}
			items = append(items, map[string]any{
				"identity_id": g.IdentityID, "level": g.Level, "roles": g.Roles, "source": "machine",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))

	// role registry: define is a full-overwrite upsert (and un-retires);
	// the list never shows retired roles
	mux.HandleFunc("GET "+prefix+"/roles", authed(func(w http.ResponseWriter, r *http.Request) {
		items := []stubRole{}
		for _, role := range s.roles {
			if !role.Retired {
				items = append(items, *role)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	mux.HandleFunc("POST "+prefix+"/roles", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		slug, ok := stubSlug(str(b, "slug"))
		if !ok {
			oops(w, http.StatusBadRequest, fmt.Sprintf("%q is not a usable role slug", str(b, "slug")))
			return
		}
		if strings.TrimSpace(str(b, "display_name")) == "" {
			oops(w, http.StatusBadRequest, "display_name is required")
			return
		}
		caps := strs(b, "capabilities")
		for _, c := range caps {
			if c != "manages_tenant" {
				oops(w, http.StatusBadRequest, fmt.Sprintf("unknown capability %q", c))
				return
			}
		}
		level := str(b, "base_level")
		if level != "" && level != "viewer" && level != "member" && level != "admin" {
			oops(w, http.StatusBadRequest, "base_level must be admin, member or viewer")
			return
		}
		s.roles[slug] = &stubRole{Slug: slug, DisplayName: str(b, "display_name"), Capabilities: caps, BaseLevel: level}
		json.NewEncoder(w).Encode(map[string]string{"status": "defined", "slug": slug})
	}))
	mux.HandleFunc("POST "+prefix+"/roles/{role}/retire", authed(func(w http.ResponseWriter, r *http.Request) {
		slug, _ := stubSlug(r.PathValue("role"))
		if role, ok := s.roles[slug]; ok {
			role.Retired = true
		}
		// a never-defined role retires to the same place — 200
		json.NewEncoder(w).Encode(map[string]string{"status": "retired"})
	}))

	// teams: create upserts display name only; a deleted slug stays
	// retired; membership is one-at-a-time; bindings replace as a list
	mux.HandleFunc("POST "+prefix+"/teams", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		slug, ok := stubSlug(str(b, "slug"))
		if !ok {
			oops(w, http.StatusBadRequest, fmt.Sprintf("%q is not a usable team slug", str(b, "slug")))
			return
		}
		tenant, _ := stubSlug(str(b, "tenant"))
		tn, exists := s.tenants[tenant]
		if !exists || tn.Status != "active" {
			oops(w, http.StatusBadRequest, "tenant does not exist")
			return
		}
		if strings.TrimSpace(str(b, "display_name")) == "" {
			oops(w, http.StatusBadRequest, "display_name is required")
			return
		}
		if t, taken := s.teams[slug]; taken {
			switch {
			case t.Deleted:
				oops(w, http.StatusBadRequest, fmt.Sprintf("team %q was deleted — its slug stays retired", slug))
			case t.Tenant != tenant:
				oops(w, http.StatusBadRequest, "team slug is taken")
			default:
				t.DisplayName = str(b, "display_name") // the terraform path re-applies
				json.NewEncoder(w).Encode(map[string]string{"status": "created", "slug": slug})
			}
			return
		}
		s.teams[slug] = &stubTeam{Slug: slug, Tenant: tenant, DisplayName: str(b, "display_name"), Members: []string{}, Bindings: []stubBinding{}}
		json.NewEncoder(w).Encode(map[string]string{"status": "created", "slug": slug})
	}))
	mux.HandleFunc("GET "+prefix+"/teams/{team}", authed(func(w http.ResponseWriter, r *http.Request) {
		slug, _ := stubSlug(r.PathValue("team"))
		t := liveTeam(slug)
		if t == nil {
			oops(w, http.StatusNotFound, "unknown team")
			return
		}
		json.NewEncoder(w).Encode(t)
	}))
	teamMember := func(add bool) http.HandlerFunc {
		return authed(func(w http.ResponseWriter, r *http.Request) {
			slug, _ := stubSlug(r.PathValue("team"))
			t := liveTeam(slug)
			if t == nil {
				oops(w, http.StatusNotFound, "unknown team")
				return
			}
			if t.IdpManaged {
				oops(w, http.StatusBadRequest, "this team's membership is managed by the identity provider")
				return
			}
			b := body(r)
			id := str(b, "identity_id")
			if id == "" {
				email := str(b, "email")
				if !strings.Contains(email, "@") {
					oops(w, http.StatusBadRequest, "identity_id or email is required")
					return
				}
				id = identity(email)
			}
			if add && !slices.Contains(t.Members, id) {
				t.Members = append(t.Members, id)
			}
			if !add {
				t.Members = slices.DeleteFunc(t.Members, func(m string) bool { return m == id })
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "identity_id": id})
		})
	}
	mux.HandleFunc("POST "+prefix+"/teams/{team}/members", teamMember(true))
	mux.HandleFunc("POST "+prefix+"/teams/{team}/members/remove", teamMember(false))
	mux.HandleFunc("POST "+prefix+"/teams/{team}/bindings", authed(func(w http.ResponseWriter, r *http.Request) {
		slug, _ := stubSlug(r.PathValue("team"))
		t := liveTeam(slug)
		if t == nil {
			oops(w, http.StatusNotFound, "unknown team")
			return
		}
		var b struct {
			Bindings []stubBinding `json:"bindings"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		for _, bind := range b.Bindings {
			if !strings.HasPrefix(bind.Ns, org+"/") {
				oops(w, http.StatusBadRequest, fmt.Sprintf("namespace must start with %q — org-level roles go through the invite flow", org+"/"))
				return
			}
			if bind.Level != "admin" && bind.Level != "member" && bind.Level != "viewer" {
				oops(w, http.StatusBadRequest, bind.Ns+": level must be admin, member or viewer")
				return
			}
			for _, role := range bind.Roles {
				if live, ok := s.roles[role]; !ok || live.Retired {
					oops(w, http.StatusBadRequest, fmt.Sprintf("role %q is not defined for this organization — declare it via POST /org/%s/roles first", role, org))
					return
				}
			}
		}
		bindings := b.Bindings
		if bindings == nil {
			bindings = []stubBinding{}
		}
		t.Bindings = bindings
		json.NewEncoder(w).Encode(map[string]string{"status": "bindings set"})
	}))
	mux.HandleFunc("POST "+prefix+"/teams/{team}/delete", authed(func(w http.ResponseWriter, r *http.Request) {
		slug, _ := stubSlug(r.PathValue("team"))
		t := liveTeam(slug)
		if t == nil {
			oops(w, http.StatusNotFound, "unknown team")
			return
		}
		t.Deleted = true
		json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	}))

	mux.HandleFunc("GET "+prefix+"/members", authed(func(w http.ResponseWriter, r *http.Request) {
		items := []map[string]string{}
		for email, role := range s.members {
			items = append(items, map[string]string{"id": uuid.NewString(), "email": email, "role": role, "status": "active"})
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	mux.HandleFunc("POST "+prefix+"/grants", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		ns := str(b, "namespace")
		// tenant grants: the live surface — sub-namespaces only, each
		// role registry-checked, convergent upsert per (identity, ns)
		if strings.HasPrefix(ns, org+"/") {
			level := str(b, "level")
			if level == "" {
				level = str(b, "role") // legacy alias
			}
			if level == "owner" {
				level = "admin"
			}
			if level != "admin" && level != "member" && level != "viewer" {
				oops(w, http.StatusBadRequest, "level must be admin, member or viewer")
				return
			}
			roles := strs(b, "roles")
			for _, role := range roles {
				if live, ok := s.roles[role]; !ok || live.Retired {
					oops(w, http.StatusBadRequest, fmt.Sprintf("role %q is not defined for this organization — declare it via POST /org/%s/roles first", role, org))
					return
				}
			}
			id := str(b, "identity_id")
			if id == "" {
				email := str(b, "email")
				if !strings.Contains(email, "@") {
					oops(w, http.StatusBadRequest, "identity_id or email is required")
					return
				}
				id = identity(email)
			}
			s.tgrants[id+"|"+ns] = &stubTGrant{IdentityID: id, Level: level, Roles: roles}
			json.NewEncoder(w).Encode(map[string]string{"status": "granted", "identity_id": id})
			return
		}
		// legacy org-membership branch — the LIVE API refuses a bare-org
		// namespace ("org-level roles go through the invite flow"); kept
		// while latchkey_grant's fate is decided so its tests still run
		s.members[strings.ToLower(str(b, "email"))] = str(b, "role")
		json.NewEncoder(w).Encode(map[string]string{"status": "granted"})
	}))
	mux.HandleFunc("POST "+prefix+"/grants/revoke", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		ns := str(b, "namespace")
		if strings.HasPrefix(ns, org+"/") {
			id := str(b, "identity_id")
			if id == "" {
				id = identity(str(b, "email"))
			}
			// idempotent — a never-granted namespace still answers 200
			if g, ok := s.tgrants[id+"|"+ns]; ok {
				g.Revoked = true
			}
			json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
			return
		}
		delete(s.members, strings.ToLower(str(b, "email")))
		json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
	}))

	return httptest.NewServer(mux)
}

// stubTemplateData is the live API's variable allowlist
// (identity/aggregates/business.go mailTemplateData): TenantName and
// Role are filled for tenant_invite only. Kept in lock-step so a
// placeholder the docs recommend is proven here, not on first apply.
type stubTemplateData struct {
	Link, Code, OrgName, Email, Brand, TenantName, Role string
}

// stubValidateTemplate mirrors aggregates.ValidateTemplate: a known
// kind, non-blank subject/body, the anchor the recipient acts on
// ({{.Code}} for login_code, {{.Link}} otherwise) in the body and any
// html part, and every part renders with missingkey=error against the
// sample data — an unknown placeholder such as {{.Tenant}} is a 400.
func stubValidateTemplate(kind, subject, body, html string) error {
	switch kind {
	case "login", "login_code", "invite", "link_email", "tenant_invite":
	default:
		return fmt.Errorf("unknown template kind (login, login_code, invite, link_email, tenant_invite)")
	}
	if strings.TrimSpace(subject) == "" || strings.TrimSpace(body) == "" {
		return fmt.Errorf("subject and body are required")
	}
	anchor, need := "{{.Link}}", "somewhere to click"
	if kind == "login_code" {
		anchor, need = "{{.Code}}", "the code"
	}
	if !strings.Contains(body, anchor) {
		return fmt.Errorf("the body must include %s — the recipient needs %s", anchor, need)
	}
	if html != "" && !strings.Contains(html, anchor) {
		return fmt.Errorf("the html must include %s — the recipient needs %s", anchor, need)
	}
	sample := stubTemplateData{Link: "https://example.test/x", Code: "123456", OrgName: "Org", Email: "a@example.test", Brand: "Brand", TenantName: "Tenant", Role: "role"}
	parts := map[string]string{"subject": subject, "body": body}
	if html != "" {
		parts["html"] = html
	}
	for name, text := range parts {
		tmpl, err := template.New(name).Option("missingkey=error").Parse(text)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := tmpl.Execute(io.Discard, sample); err != nil {
			return fmt.Errorf("%s: %w (available: {{.Link}}, {{.Code}}, {{.OrgName}}, {{.Email}}, {{.Brand}}, {{.TenantName}}, {{.Role}})", name, err)
		}
	}
	return nil
}
