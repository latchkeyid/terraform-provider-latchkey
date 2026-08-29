package provider

// A faithful in-memory stub of Latchkey's org API, so acceptance tests
// drive the real terraform CLI against real HTTP without a database.
// Point LATCHKEY_ISSUER at a live instance instead to run the same tests
// against the real service.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

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

type stubState struct {
	mu        sync.Mutex
	clients   map[string]*stubClient
	templates map[string]stubTemplate
	domains   map[string]bool
	members   map[string]string // email → role
	branding  map[string]string
	bgStyle   map[string]string
	keys      map[string][]*stubKey // tenant → ledger
	keySeq    int
}

func newStub(org string) *httptest.Server {
	s := &stubState{
		clients:   map[string]*stubClient{},
		templates: map[string]stubTemplate{},
		domains:   map[string]bool{},
		members:   map[string]string{},
		branding:  map[string]string{},
		bgStyle:   map[string]string{"brand_bg_fit": "", "brand_bg_position": "", "brand_bg_scrim": ""},
		keys:      map[string][]*stubKey{},
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
		key := fmt.Sprintf("%sSTUB%02drandomrandomrandomrandCHKSUM", marker, s.keySeq)
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
		if kind != "login" && kind != "invite" && kind != "link_email" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "unknown template kind"})
			return
		}
		s.templates[kind] = stubTemplate{Kind: kind, Subject: str(b, "subject"), Body: str(b, "body"), HTML: str(b, "html")}
		json.NewEncoder(w).Encode(map[string]string{"status": "saved"})
	}))
	mux.HandleFunc("POST "+prefix+"/templates/clear", authed(func(w http.ResponseWriter, r *http.Request) {
		delete(s.templates, str(body(r), "kind"))
		json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
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

	mux.HandleFunc("GET "+prefix+"/members", authed(func(w http.ResponseWriter, r *http.Request) {
		items := []map[string]string{}
		for email, role := range s.members {
			items = append(items, map[string]string{"id": uuid.NewString(), "email": email, "role": role, "status": "active"})
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	mux.HandleFunc("POST "+prefix+"/grants", authed(func(w http.ResponseWriter, r *http.Request) {
		b := body(r)
		s.members[strings.ToLower(str(b, "email"))] = str(b, "role")
		json.NewEncoder(w).Encode(map[string]string{"status": "granted"})
	}))
	mux.HandleFunc("POST "+prefix+"/grants/revoke", authed(func(w http.ResponseWriter, r *http.Request) {
		delete(s.members, strings.ToLower(str(body(r), "email")))
		json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
	}))

	return httptest.NewServer(mux)
}
